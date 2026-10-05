package consensus

import (
	"bytes"
	"fmt"
	"testing"
)

// 本文件固定“节点配置在初始化后不可变”的回归预期：
// 创建节点时保存的种子与验证者名单是调用方数据的副本，Config 返回的也是副本。
// 调用方改写自己手中的种子、名单或查询结果，都不能改变节点再次查询得到的
// 原始配置、按轮次计算的提议者，以及投票时的名单判定与确认阈值。

// copyValidatorList 深拷贝验证者名单，用于在调用方改写前保存原始顺序的副本。
func copyValidatorList(vals [][]byte) [][]byte {
	out := make([][]byte, len(vals))
	for i, v := range vals {
		out[i] = append([]byte(nil), v...)
	}
	return out
}

// assertConfigSnapshot 校验配置查询结果与初始化时的种子、名单（含顺序）和数值一致。
func assertConfigSnapshot(t *testing.T, got ConfigSnapshot, wantSeed []byte, wantVals [][]byte, wantMaxTxs, wantCapacity uint64) {
	t.Helper()
	if !bytes.Equal(got.Seed, wantSeed) {
		t.Fatalf("config seed = %x, want original %x", got.Seed, wantSeed)
	}
	if len(got.Validators) != len(wantVals) {
		t.Fatalf("config validators = %d members, want %d", len(got.Validators), len(wantVals))
	}
	for i, v := range wantVals {
		if !bytes.Equal(got.Validators[i], v) {
			t.Fatalf("config validator %d = %x, want original %x", i, got.Validators[i], v)
		}
	}
	if got.MaxTxsPerBlock != wantMaxTxs {
		t.Fatalf("config max txs per block = %d, want %d", got.MaxTxsPerBlock, wantMaxTxs)
	}
	if got.PoolCapacity != wantCapacity {
		t.Fatalf("config pool capacity = %d, want %d", got.PoolCapacity, wantCapacity)
	}
}

// 节点创建后，调用方复用最初的种子字节与验证者名单并改写：
// 改种子内容、改名单内某个公钥、换入新成员、调整成员顺序。
// 节点再次查询得到的仍是原始种子、原始名单及其顺序；
// 按指定轮次查询提议者也仍得到原配置对应的公钥。
func TestFixedConfigSurvivesCallerInputEdits(t *testing.T) {
	keys := genKeys(t, 4)
	seed := []byte("fixed-config-seed-v1")
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	// 原始种子与原始名单（含顺序）的独立副本，作为预期基准。
	wantSeed := append([]byte(nil), seed...)
	wantVals := copyValidatorList(vals)
	outsider := genKeys(t, 1)[0]

	n, err := New(t.TempDir(), Config{Seed: seed, Validators: vals, MaxTxsPerBlock: 3, PoolCapacity: 5})
	if err != nil {
		t.Fatal(err)
	}

	// 调用方改写手中的种子与名单：改内容、改公钥、换成员、调顺序。
	seed[0] ^= 0xff
	seed[len(seed)-1] ^= 0xff
	vals[1][0] ^= 0xff
	vals[2] = outsider.pub
	vals[0], vals[3] = vals[3], vals[0]

	// 再查询配置：原始种子、原始名单及顺序、原始数值都不变。
	assertConfigSnapshot(t, n.Config(), wantSeed, wantVals, 3, 5)

	// 按指定轮次查询提议者，仍得到原配置对应的公钥。
	for _, round := range []uint64{1, 2, 7, 255, 256, 4294967297} {
		want := proposerAt(wantSeed, wantVals, round)
		if got := n.ProposerFor(round); !bytes.Equal(got, want) {
			t.Fatalf("ProposerFor(%d) = %x after caller edits, want %x from original config", round, got, want)
		}
	}

	// 提议者查询结果同样是副本：改写它不影响再次查询。
	p := n.ProposerFor(1)
	for i := range p {
		p[i] ^= 0xff
	}
	if got := n.ProposerFor(1); !bytes.Equal(got, proposerAt(wantSeed, wantVals, 1)) {
		t.Fatalf("ProposerFor(1) changed after caller mutated a previous result: %x", got)
	}
}

// Config 的查询结果是独立副本：编辑其中的种子、某个验证者公钥，
// 替换或增减名单成员、改动数值后，再取一次配置仍看到原来的值；
// 先后取得的两份结果彼此独立，先取得的不因另一份被改写而变化。
// 单块交易上限与池容量仍是初始化时的数值：改查询结果里的数值
// 不能成为修改节点配置的入口，打包上限与池容量判断都不受影响。
func TestConfigSnapshotIsIndependentCopy(t *testing.T) {
	keys := genKeys(t, 4)
	seed := []byte("config-snapshot-seed")
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = append([]byte(nil), k.pub...)
	}
	wantSeed := append([]byte(nil), seed...)
	wantVals := copyValidatorList(vals)
	outsider := genKeys(t, 1)[0]

	n, err := New(t.TempDir(), Config{Seed: seed, Validators: vals, MaxTxsPerBlock: 1, PoolCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}

	first := n.Config()
	second := n.Config()

	// 调用方尽力改写第一份结果：种子、公钥、换成员、减成员、调顺序、改数值。
	first.Seed[0] ^= 0xff
	first.Validators[0][0] ^= 0xff
	first.Validators[1] = outsider.pub
	first.Validators = append(first.Validators[:2], first.Validators[3:]...) // 减掉一名成员
	first.Validators[0], first.Validators[len(first.Validators)-1] = first.Validators[len(first.Validators)-1], first.Validators[0]
	first.MaxTxsPerBlock = 100
	first.PoolCapacity = 100

	// 第二份结果保持取得时的原值，不随第一份被改写而变化。
	assertConfigSnapshot(t, second, wantSeed, wantVals, 1, 2)
	// 节点再取配置，仍是初始化时的值。
	assertConfigSnapshot(t, n.Config(), wantSeed, wantVals, 1, 2)
	// 提议者仍按原配置计算。
	if got := n.ProposerFor(1); !bytes.Equal(got, proposerAt(wantSeed, wantVals, 1)) {
		t.Fatalf("ProposerFor(1) = %x after snapshot edits, want %x", got, proposerAt(wantSeed, wantVals, 1))
	}

	// 池容量仍是初始化时的 2：改查询结果里的数值不能放大容量。
	tx1 := NewTransaction(keys[0].priv, 1, []byte("a"), 5, 100)
	tx2 := NewTransaction(keys[1].priv, 1, []byte("b"), 4, 100)
	if _, err := n.Submit(tx1); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(tx2); err != nil {
		t.Fatal(err)
	}
	low := NewTransaction(keys[2].priv, 1, []byte("c"), 1, 100)
	if _, err := n.Submit(low); reason(err) != ReasonPoolFull {
		t.Fatalf("third submit got %v, want %s (pool capacity must stay 2)", err, ReasonPoolFull)
	}

	// 单块上限仍是初始化时的 1：改查询结果里的数值不能放大打包上限。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TxIDs) != 1 || p.TxIDs[0] != tx1.ID() {
		t.Fatalf("proposal = %v, want only highest-fee %s (max txs per block must stay 1)", p.TxIDs, tx1.ID())
	}
}

// 固定名单还要体现在正常投票上：四名验证者的节点产生提议后，
// 即使外部名单或配置副本被编辑，原名单中的验证者仍可投票；
// 外部新换入的身份以 not-validator 拒绝且不计票，
// 两名原验证者的票不足以确认，第三名原验证者的票才能确认。
// 被拒绝的身份不出现在候选投票者中，也不使确认提前发生。
func TestVotingUsesFixedValidatorSet(t *testing.T) {
	keys := genKeys(t, 4)
	// 输入名单用独立切片，keys 中的公钥保持原样用于投票。
	inputVals := make([][]byte, len(keys))
	for i, k := range keys {
		inputVals[i] = append([]byte(nil), k.pub...)
	}
	outsider := genKeys(t, 1)[0]

	n, err := New(t.TempDir(), Config{Seed: []byte("fixed-voting-seed"), Validators: inputVals, MaxTxsPerBlock: 10})
	if err != nil {
		t.Fatal(err)
	}
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	round := n.CurrentRound()

	// 创建后外部编辑：输入名单换入新身份、改公钥；配置副本同样被改写。
	inputVals[0] = outsider.pub
	inputVals[1][0] ^= 0xff
	snap := n.Config()
	snap.Validators[2] = outsider.pub
	snap.Validators[3][0] ^= 0xff
	snap.Seed[0] ^= 0xff

	// 外部新换入的身份以 not-validator 拒绝，不计票。
	if _, err := n.Vote(outsider.pub, round, p.BlockID); reason(err) != ReasonNotValidator {
		t.Fatalf("outsider vote got %v, want %s", err, ReasonNotValidator)
	}

	// 原名单中的验证者仍可投票；两票不足以确认（4 人需超过 2/3，即 3 票）。
	res, err := n.Vote(keys[0].pub, round, p.BlockID)
	if err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("first original validator vote: %+v %v", res, err)
	}
	res, err = n.Vote(keys[1].pub, round, p.BlockID)
	if err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("second original validator vote: %+v %v", res, err)
	}
	if n.Height() != 0 {
		t.Fatal("two of four votes must not confirm")
	}

	// 被拒绝的身份没有出现在候选投票者中，也没有使确认提前发生。
	rc, err := n.Candidates(round)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(rc.Candidates))
	}
	cand := rc.Candidates[0]
	if len(cand.Voters) != 2 {
		t.Fatalf("voters = %d, want 2 (rejected identity must not be counted)", len(cand.Voters))
	}
	for _, v := range cand.Voters {
		if bytes.Equal(v, outsider.pub) {
			t.Fatal("rejected outsider must not appear among candidate voters")
		}
	}
	// 未投票者仍是原名单中的另外两人，不含换入的身份。
	if len(rc.Unvoted) != 2 {
		t.Fatalf("unvoted = %d, want 2", len(rc.Unvoted))
	}
	unvoted := map[string]bool{}
	for _, v := range rc.Unvoted {
		unvoted[fmt.Sprintf("%x", v)] = true
	}
	if !unvoted[fmt.Sprintf("%x", keys[2].pub)] || !unvoted[fmt.Sprintf("%x", keys[3].pub)] {
		t.Fatalf("unvoted should be the two remaining original validators: %v", rc.Unvoted)
	}
	if unvoted[fmt.Sprintf("%x", outsider.pub)] {
		t.Fatal("outsider must not appear among unvoted validators")
	}

	// 第三名原验证者的票才能确认。
	res, err = n.Vote(keys[2].pub, round, p.BlockID)
	if err != nil || !res.Confirmed || res.Block == nil {
		t.Fatalf("third original validator vote should confirm: %+v %v", res, err)
	}
	if n.Height() != 1 {
		t.Fatalf("height = %d, want 1 after third vote", n.Height())
	}
}
