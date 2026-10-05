package consensus

import (
	"bytes"
	"fmt"
	"testing"
)

// 本文件回归保障节点的固定配置：New 成功时保存种子与验证者名单（含顺序），
// Config 只返回配置副本。节点规则在初始化后固定，调用方复用并改写最初传入的
// 种子字节与名单、编辑 Config 返回的副本，都只能改变自己持有的内容；
// 再次查询仍是原种子、原名单与原顺序，指定轮次的提议者仍是原配置对应的公钥，
// 单块上限与池容量仍是初始化数值，正常投票仍只承认原名单中的身份。

// assertConfigFixed 校验配置快照与初始化时固定的取值逐字节一致：
// 种子、验证者成员与顺序、单块上限与池容量。
func assertConfigFixed(t *testing.T, got ConfigSnapshot, seed []byte, vals [][]byte, maxTxs, capacity uint64) {
	t.Helper()
	if !bytes.Equal(got.Seed, seed) {
		t.Fatalf("config seed = %x, want %x", got.Seed, seed)
	}
	if len(got.Validators) != len(vals) {
		t.Fatalf("config validators len = %d, want %d", len(got.Validators), len(vals))
	}
	for i := range vals {
		if !bytes.Equal(got.Validators[i], vals[i]) {
			t.Fatalf("config validator[%d] = %x, want %x (membership and order must stay fixed)",
				i, got.Validators[i], vals[i])
		}
	}
	if got.MaxTxsPerBlock != maxTxs {
		t.Fatalf("config max txs = %d, want %d", got.MaxTxsPerBlock, maxTxs)
	}
	if got.PoolCapacity != capacity {
		t.Fatalf("config pool capacity = %d, want %d", got.PoolCapacity, capacity)
	}
}

// newFixedConfigNode 用调用方持有的独立种子字节与名单字节建节点。
// seed/vals 就是实际传给 New 的两个切片（调用方随后可以复用并直接改写它们）；
// origSeed/origVals 是初始化时固定取值的独立留档副本，供改写后对比。
func newFixedConfigNode(t *testing.T, keys []testKey, maxTxs, capacity uint64) (
	n *Node, dir string, seed []byte, vals [][]byte, origSeed []byte, origVals [][]byte,
) {
	t.Helper()
	dir = t.TempDir()
	seed = append([]byte(nil), []byte("fixed-config-seed-v1")...)
	vals = make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = append([]byte(nil), k.pub...)
	}
	n, err := New(dir, Config{Seed: seed, Validators: vals, MaxTxsPerBlock: maxTxs, PoolCapacity: capacity})
	if err != nil {
		t.Fatal(err)
	}
	origSeed = append([]byte(nil), seed...)
	origVals = make([][]byte, len(vals))
	for i := range vals {
		origVals[i] = append([]byte(nil), vals[i]...)
	}
	return n, dir, seed, vals, origSeed, origVals
}

// New 成功后调用方复用最初的种子字节与名单：改写种子内容、改写名单内某个
// 公钥、交换成员顺序、替换成员、追加成员，都不能改变节点已固定的配置。
// 再次 Config 得到原种子、原名单及顺序；指定轮次的提议者仍是原配置
// （原种子 + 原顺序）按公开规则确定的公钥。
func TestNewCopiesSeedAndValidators(t *testing.T) {
	keys := genKeys(t, 4)
	n, _, seed, vals, origSeed, origVals := newFixedConfigNode(t, keys, 7, 5)

	// 先取一份提议者钉住值（该种子下第 1 轮在 idx0、第 3 轮在 idx1，
	// 由独立复算预先确定，见文件外的 propcalc 复算）。
	prop1 := n.ProposerFor(1)
	prop3 := n.ProposerFor(3)
	prop256 := n.ProposerFor(256)
	if string(prop1) != string(origVals[0]) {
		t.Fatalf("setup: round 1 proposer = %x, want validator 0 %x", prop1, origVals[0])
	}
	if string(prop3) != string(origVals[1]) {
		t.Fatalf("setup: round 3 proposer = %x, want validator 1 %x", prop3, origVals[1])
	}
	if string(prop256) != string(origVals[1]) {
		t.Fatalf("setup: round 256 proposer = %x, want validator 1 %x", prop256, origVals[1])
	}

	// 调用方直接复用并改写传给 New 的那两个切片：种子逐字节翻转。
	for i := range seed {
		seed[i] ^= 0xff
	}
	// 名单：改写一名成员公钥、交换两名成员顺序、用名单外身份替换一名成员、追加一名成员。
	vals[1][0] ^= 0xff
	vals[0], vals[2] = vals[2], vals[0]
	outsider := genKeys(t, 1)[0].pub
	vals[3] = append([]byte(nil), outsider...)
	vals = append(vals, make([]byte, 32))

	// 节点再次查询，配置仍是初始化时的原种子、原名单与原顺序。
	assertConfigFixed(t, n.Config(), origSeed, origVals, 7, 5)

	// 指定轮次的提议者仍由原种子与原名单顺序决定；成员相同但次序变化
	// 会改变提议者规则，这里钉住的公钥不能随调用方交换顺序而变化。
	for _, c := range []struct {
		round uint64
		idx   int
	}{{1, 0}, {2, 0}, {3, 1}, {255, 0}, {256, 1}, {257, 3}} {
		got := n.ProposerFor(c.round)
		if string(got) != string(origVals[c.idx]) {
			t.Fatalf("ProposerFor(%d) after caller list edit = %x, want original validator %d %x",
				c.round, got, c.idx, origVals[c.idx])
		}
		if string(got) != string(wantProposerByRule(origSeed, origVals, c.round)) {
			t.Fatalf("ProposerFor(%d) = %x, independent rule computation over original config gives %x",
				c.round, got, wantProposerByRule(origSeed, origVals, c.round))
		}
	}
	if string(n.CurrentProposer()) != string(origVals[0]) {
		t.Fatalf("CurrentProposer = %x, want original validator 0", n.CurrentProposer())
	}

	// 名单外身份不能借调用方替换成员进入节点：投票按 not-validator 拒绝。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Vote(outsider, 1, p.BlockID); reason(err) != ReasonNotValidator {
		t.Fatalf("swapped-in outsider vote got %v, want %s", err, ReasonNotValidator)
	}
}

// Config 返回的是独立副本：调用方编辑其中的种子与某个验证者公钥，替换、
// 增减手中名单的成员，改写单块上限与池容量，再取一次配置仍看到原值；
// 分别取得的两份结果彼此独立，先取得的结果不会因为另一份被改写而变化。
func TestConfigResultsAreIndependentCopies(t *testing.T) {
	keys := genKeys(t, 4)
	n, _, _, _, origSeed, origVals := newFixedConfigNode(t, keys, 7, 5)

	first := n.Config()
	second := n.Config()
	assertConfigFixed(t, first, origSeed, origVals, 7, 5)
	assertConfigFixed(t, second, origSeed, origVals, 7, 5)

	// 改写先取得的一份：种子、公钥字节、名单成员与长度、两个数值字段。
	for i := range first.Seed {
		first.Seed[i] ^= 0xaa
	}
	first.Validators[0][0] ^= 0xff
	first.Validators[1] = append([]byte(nil), genKeys(t, 1)[0].pub...)
	first.Validators[2], first.Validators[3] = first.Validators[3], first.Validators[2]
	first.Validators = append(first.Validators, make([]byte, 32))
	first.Validators = first.Validators[:2]
	first.MaxTxsPerBlock = 999
	first.PoolCapacity = 999

	// 另一份早已取得的结果与新取得的结果都保持原值。
	assertConfigFixed(t, second, origSeed, origVals, 7, 5)
	assertConfigFixed(t, n.Config(), origSeed, origVals, 7, 5)

	// 再改写第二份（含直接重切片与覆写字节），第一份已被编辑的内容不由节点负责，
	// 但新查询与另一份未编辑部分仍必须是原配置。
	second.Seed[0] ^= 0xff
	second.Validators[0] = append([]byte(nil), genKeys(t, 1)[0].pub...)
	second.Validators = append(second.Validators, make([]byte, 32), make([]byte, 32))
	second.MaxTxsPerBlock = 1
	second.PoolCapacity = 0
	fresh := n.Config()
	assertConfigFixed(t, fresh, origSeed, origVals, 7, 5)

	// 第三份独立于前两份：改写第三份后再取，节点与先前列出的原成员字节均不变。
	for i := range fresh.Seed {
		fresh.Seed[i] = 0
	}
	fresh.Validators[0][0] ^= 0xff
	fresh.Validators = nil
	again := n.Config()
	assertConfigFixed(t, again, origSeed, origVals, 7, 5)
	// 第二份中未覆写的成员仍是取得时的原字节，不受第三份编辑影响。
	if !bytes.Equal(second.Validators[1], origVals[1]) || !bytes.Equal(second.Validators[2], origVals[2]) {
		t.Fatalf("earlier Config result changed by editing a later result: %x %x",
			second.Validators[1], second.Validators[2])
	}
}

// 投票流程中的隔离：四名验证者的节点产生提议后，即使调用方手中的外部名单
// 与 Config 副本被编辑，原名单验证者仍可投票，外部换入身份以 not-validator
// 拒绝且不计票、不进候选投票者、不使确认提前；两名原验证者的票不足以确认，
// 第三名原验证者的票才确认。固定配置在确认后与重开后都保持原样。
func TestFixedConfigHoldsDuringVoting(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir, _, _, origSeed, origVals := newFixedConfigNode(t, keys, 4, 3)

	// 建节点后先固定第 1、2 轮提议者（取得时的副本）。
	prop1 := append([]byte(nil), n.ProposerFor(1)...)
	prop2 := append([]byte(nil), n.ProposerFor(2)...)

	p, err := n.Propose() // 空池产生空块提议。
	if err != nil {
		t.Fatal(err)
	}

	// 调用方编辑 Config 副本：种子、名单成员、顺序与长度、两个数值字段全部改写。
	cfg := n.Config()
	for i := range cfg.Seed {
		cfg.Seed[i] ^= 0xff
	}
	cfg.Validators[0][0] ^= 0xff
	cfg.Validators[1], cfg.Validators[3] = cfg.Validators[3], cfg.Validators[1]
	cfg.Validators = append(cfg.Validators, make([]byte, 32))
	cfg.MaxTxsPerBlock = 1
	cfg.PoolCapacity = 0

	// 调用方另造一份外部名单并改写：交换顺序、把成员换成名单外身份。
	outsider := genKeys(t, 1)[0]
	external := make([][]byte, len(keys))
	for i, k := range keys {
		external[i] = append([]byte(nil), k.pub...)
	}
	external[0], external[2] = external[2], external[0]
	external[1] = append([]byte(nil), outsider.pub...)

	// 外部换入身份投票：not-validator 拒绝，候选投票者中没有它，也不占任何名额。
	if _, err := n.Vote(outsider.pub, 1, p.BlockID); reason(err) != ReasonNotValidator {
		t.Fatalf("outsider vote got %v, want %s", err, ReasonNotValidator)
	}
	rc := mustCandidates(t, n, 1)
	local := candidateByBlockID(rc, p.BlockID)
	if local == nil || len(local.Voters) != 0 {
		t.Fatalf("rejected outsider vote must not count: %+v", local)
	}
	if len(rc.Unvoted) != 4 {
		t.Fatalf("outsider must not appear among candidate voters; unvoted = %d, want 4", len(rc.Unvoted))
	}

	// 两名原名单验证者投票：都计入，但 2/4 不超过 2/3，不能确认。
	r1, err := n.Vote(keys[0].pub, 1, p.BlockID)
	if err != nil || !r1.Counted || r1.Confirmed {
		t.Fatalf("original validator vote 1 = %+v, %v", r1, err)
	}
	r2, err := n.Vote(keys[1].pub, 1, p.BlockID)
	if err != nil || !r2.Counted || r2.Confirmed {
		t.Fatalf("original validator vote 2 = %+v, %v", r2, err)
	}
	rc = mustCandidates(t, n, 1)
	local = candidateByBlockID(rc, p.BlockID)
	if len(local.Voters) != 2 {
		t.Fatalf("votes = %d, want 2", len(local.Voters))
	}
	unvoted := map[string]bool{}
	for _, v := range rc.Unvoted {
		unvoted[fmt.Sprintf("%x", v)] = true
	}
	if !unvoted[fmt.Sprintf("%x", keys[2].pub)] || !unvoted[fmt.Sprintf("%x", keys[3].pub)] || len(rc.Unvoted) != 2 {
		t.Fatalf("unvoted after 2 votes = %x, want original validators 2 and 3 only", rc.Unvoted)
	}

	// 再次编辑手中的外部名单与配置副本，外部身份仍被拒绝且不能使确认提前发生。
	external[2] = append([]byte(nil), outsider.pub...)
	cfg.Validators[0] = append([]byte(nil), outsider.pub...)
	if _, err := n.Vote(outsider.pub, 1, p.BlockID); reason(err) != ReasonNotValidator {
		t.Fatalf("outsider second vote got %v, want %s", err, ReasonNotValidator)
	}
	rc = mustCandidates(t, n, 1)
	if c := candidateByBlockID(rc, p.BlockID); len(c.Voters) != 2 {
		t.Fatalf("rejected vote must not advance confirmation: votes = %d, want 2", len(c.Voters))
	}

	// 第三名原名单验证者的票才确认（严格超过 2/3）。
	r3, err := n.Vote(keys[2].pub, 1, p.BlockID)
	if err != nil || !r3.Confirmed || r3.Block == nil {
		t.Fatalf("third original validator vote = %+v, %v, want confirmation", r3, err)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("state after confirm round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}

	// 胜出候选的投票者恰好是三名原名单验证者：被拒绝身份没有混入。
	rc = mustCandidates(t, n, 1)
	won := candidateByBlockID(rc, p.BlockID)
	if won == nil || won.Result != CandidateWon || len(won.Voters) != 3 {
		t.Fatalf("winner candidate wrong: %+v", won)
	}
	wantVoters := map[string]bool{
		fmt.Sprintf("%x", keys[0].pub): true,
		fmt.Sprintf("%x", keys[1].pub): true,
		fmt.Sprintf("%x", keys[2].pub): true,
	}
	for _, v := range won.Voters {
		if !wantVoters[fmt.Sprintf("%x", v)] {
			t.Fatalf("unexpected voter %x in confirmed block", v)
		}
	}

	// 确认后配置与各轮提议者仍是原配置。
	assertConfigFixed(t, n.Config(), origSeed, origVals, 4, 3)
	if !bytes.Equal(n.ProposerFor(1), prop1) || !bytes.Equal(n.ProposerFor(2), prop2) {
		t.Fatal("proposer for a fixed round changed after caller edits and confirmation")
	}
	if string(wantProposerByRule(origSeed, origVals, 2)) != string(prop2) {
		t.Fatalf("round 2 proposer %x does not follow the original-config rule %x",
			prop2, wantProposerByRule(origSeed, origVals, 2))
	}

	// 重开节点：固定配置原样恢复，外部身份仍被拒绝，原名单可继续正常确认。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertConfigFixed(t, reopened.Config(), origSeed, origVals, 4, 3)
	p2, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Vote(outsider.pub, 2, p2.BlockID); reason(err) != ReasonNotValidator {
		t.Fatalf("outsider vote after reopen got %v, want %s", err, ReasonNotValidator)
	}
	v1, err := reopened.Vote(keys[0].pub, 2, p2.BlockID)
	if err != nil || v1.Confirmed {
		t.Fatalf("reopened vote 1 = %+v, %v", v1, err)
	}
	if _, err := reopened.Vote(keys[1].pub, 2, p2.BlockID); err != nil {
		t.Fatal(err)
	}
	v3, err := reopened.Vote(keys[2].pub, 2, p2.BlockID)
	if err != nil || !v3.Confirmed {
		t.Fatalf("reopened node must confirm with three original validators: %+v, %v", v3, err)
	}
	if reopened.Height() != 2 || reopened.CurrentRound() != 3 {
		t.Fatalf("state after reopen confirm height=%d round=%d, want 2/3",
			reopened.Height(), reopened.CurrentRound())
	}
}

// 改写 Config 结果里的单块上限与池容量不是修改节点配置的入口：
// 节点仍按初始化时的上限 1 打包、按容量 2 拒入新交易。
func TestConfigValueEditsDoNotChangeEnforcement(t *testing.T) {
	keys := genKeys(t, 4)
	n, _, _, _, origSeed, origVals := newFixedConfigNode(t, keys, 1, 2)

	cfg := n.Config()
	cfg.MaxTxsPerBlock = 999
	cfg.PoolCapacity = 999
	cfg.Seed[0] ^= 0xff
	cfg.Validators[0][0] ^= 0xff

	// 容量 2：两笔占满，第三笔费用 0 挤不脱费用 1 的排队交易，按 pool-full 拒绝。
	tx1 := NewTransaction(keys[0].priv, 1, []byte("one"), 1, 100)
	tx2 := NewTransaction(keys[1].priv, 1, []byte("two"), 2, 100)
	for _, tx := range []*Transaction{tx1, tx2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	tx3 := NewTransaction(keys[2].priv, 1, []byte("three"), 0, 100)
	if _, err := n.Submit(tx3); reason(err) != ReasonPoolFull {
		t.Fatalf("submit after capacity result edit got %v, want %s", err, ReasonPoolFull)
	}

	// 单块上限仍为 1：即使调用方把结果改成 999，提议只打包费用最高的一笔 tx2。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TxIDs) != 1 || p.TxIDs[0] != tx2.ID() {
		t.Fatalf("proposal = %v, want only [%s] (initial max-txs must still apply)", p.TxIDs, tx2.ID())
	}

	// 再次查询仍是初始化数值与原配置，编辑结果没有成为配置入口。
	assertConfigFixed(t, n.Config(), origSeed, origVals, 1, 2)
}
