package consensus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 登记其他候选并投票确认：候选使用当前轮次与下一高度，接在最新确认块之后。
func TestCandidateRegisterAndConfirm(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 20, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 30, 100)
	for _, tx := range []*Transaction{a1, a2, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	// 本地提议顺序为 [b1, a1, a2]（按费用，a2 须在 a1 之后）。
	alt, err := n.RegisterCandidate(1, []string{a1.ID(), a2.ID(), b1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID == local.BlockID {
		t.Fatal("alternative candidate must differ from local proposal")
	}
	if alt.Local {
		t.Fatal("registered candidate should not be marked local")
	}

	// 投 3 票给候选并确认。
	for i := 0; i < 3; i++ {
		res, err := n.Vote(keys[i].pub, 1, alt.BlockID)
		if err != nil {
			t.Fatalf("vote %d: %v", i, err)
		}
		if i == 2 {
			if !res.Confirmed || res.Block == nil {
				t.Fatal("third vote should confirm")
			}
			if res.Block.ID != alt.BlockID {
				t.Fatal("confirmed block id mismatch")
			}
		}
	}
	if n.CurrentRound() != 2 {
		t.Fatalf("round = %d, want 2", n.CurrentRound())
	}
	b, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != alt.BlockID || b.PreviousID != "" || b.Height != 1 || b.Round != 1 {
		t.Fatalf("block wrong: %+v", b)
	}
	want := []string{a1.ID(), a2.ID(), b1.ID()}
	if fmt.Sprint(b.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("block order = %v, want %v", b.TxIDs, want)
	}
	// 交易确认一次。
	for _, id := range want {
		info, _ := n.Tx(id)
		if info.Status != StatusConfirmed {
			t.Fatalf("tx %s status = %s, want confirmed", id, info.Status)
		}
	}
	// 账户序号按实际确认结果更新。
	if a := n.Account(keys[0].pub); a.ConfirmedSequence != 2 {
		t.Fatalf("A confirmed = %d, want 2", a.ConfirmedSequence)
	}
	if a := n.Account(keys[1].pub); a.ConfirmedSequence != 1 {
		t.Fatalf("B confirmed = %d, want 1", a.ConfirmedSequence)
	}
}

// 登记候选的各种失败原因，节点状态不变。
func TestCandidateRegisterRejections(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 2)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 20, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 30, 100)
	for _, tx := range []*Transaction{a1, a2, b1} {
		n.Submit(tx)
	}

	// 未产生本地提议。
	if _, err := n.RegisterCandidate(1, []string{a1.ID()}); reason(err) != ReasonNoProposal {
		t.Fatalf("got %v, want %s", err, ReasonNoProposal)
	}

	local, _ := n.Propose()

	// 指定非当前轮次。
	if _, err := n.RegisterCandidate(2, []string{a1.ID()}); reason(err) != ReasonWrongRound {
		t.Fatalf("got %v, want %s", err, ReasonWrongRound)
	}
	// 重复交易。
	if _, err := n.RegisterCandidate(1, []string{a1.ID(), a1.ID()}); reason(err) != ReasonDuplicateTxInCandidate {
		t.Fatalf("got %v, want %s", err, ReasonDuplicateTxInCandidate)
	}
	// 超过上限（上限 2）。
	if _, err := n.RegisterCandidate(1, []string{a1.ID(), a2.ID(), b1.ID()}); reason(err) != ReasonTooManyTxs {
		t.Fatalf("got %v, want %s", err, ReasonTooManyTxs)
	}
	// 未知交易。
	if _, err := n.RegisterCandidate(1, []string{"deadbeef"}); reason(err) != ReasonUnknownTx {
		t.Fatalf("got %v, want %s", err, ReasonUnknownTx)
	}
	// 序号不连续（A 已确认 0，候选却从 seq 2 开始）。
	if _, err := n.RegisterCandidate(1, []string{a2.ID()}); reason(err) != ReasonSequenceGap {
		t.Fatalf("got %v, want %s", err, ReasonSequenceGap)
	}
	// 状态不变：本地提议仍是唯一候选。
	rc, err := n.RoundCandidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 1 || rc.Candidates[0].BlockID != local.BlockID {
		t.Fatalf("state changed after failed registrations: %+v", rc.Candidates)
	}
}

// 已退出池的交易不能登记为候选。
func TestCandidateRegisterTxNotInPool(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	n.Submit(a1)
	n.Propose()
	// 确认 a1 使其退出池子。
	confirmByVotes(t, n, keys, mustLocalBlockID(t, n))

	// 新的一轮先产生本地提议（空块），a1 已确认不在池中，不能登记为候选。
	n.Propose()
	if _, err := n.RegisterCandidate(2, []string{a1.ID()}); reason(err) != ReasonTxNotInPool {
		t.Fatalf("got %v, want %s", err, ReasonTxNotInPool)
	}
}

func mustLocalBlockID(t *testing.T, n *Node) string {
	t.Helper()
	p, ok := n.Proposal()
	if !ok {
		t.Fatal("no proposal")
	}
	return p.BlockID
}

// 相同列表重复登记返回同一候选并保留已有票数；与本地提议同列表时返回本地候选。
func TestCandidateRegisterDedupAndVotes(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 20, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose()

	// 与本地提议相同列表 → 返回本地候选。
	same, err := n.RegisterCandidate(1, local.TxIDs)
	if err != nil {
		t.Fatal(err)
	}
	if same.BlockID != local.BlockID || !same.Local {
		t.Fatal("same list as local proposal must return local candidate")
	}

	// 登记一个不同顺序的候选。
	alt, err := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID == local.BlockID {
		t.Fatal("different order must yield different block id")
	}
	// 投一票。
	if _, err := n.Vote(keys[0].pub, 1, alt.BlockID); err != nil {
		t.Fatal(err)
	}
	// 重复登记同一列表 → 同一候选，票数保留。
	again, err := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if again.BlockID != alt.BlockID {
		t.Fatal("repeated list must return same candidate")
	}
	if len(again.Voters) != 1 || string(again.Voters[0]) != string(keys[0].pub) {
		t.Fatalf("votes not preserved: %+v", again.Voters)
	}
}

// 改投其他候选明确拒绝，原票保留；重复投同一候选不增票。
func TestCandidateVoteSwitchRejected(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 20, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose()
	alt, _ := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()})

	// keys[0] 投给本地候选。
	if _, err := n.Vote(keys[0].pub, 1, local.BlockID); err != nil {
		t.Fatal(err)
	}
	// 改投 alt → 拒绝。
	if _, err := n.Vote(keys[0].pub, 1, alt.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("got %v, want %s", err, ReasonAlreadyVoted)
	}
	// 原票保留：本地候选仍有 keys[0] 一票。
	rc, _ := n.RoundCandidates(1)
	for _, c := range rc.Candidates {
		if c.BlockID == local.BlockID {
			if len(c.Voters) != 1 {
				t.Fatalf("original vote not preserved: %+v", c.Voters)
			}
		}
	}
	// 重复投同一候选不增票。
	res, err := n.Vote(keys[0].pub, 1, local.BlockID)
	if err != nil || res.Counted {
		t.Fatalf("duplicate vote: %+v %v", res, err)
	}
	// 投给未登记的区块标识 → 未知区块。
	if _, err := n.Vote(keys[1].pub, 1, "deadbeef"); reason(err) != ReasonWrongBlock {
		t.Fatalf("got %v, want %s", err, ReasonWrongBlock)
	}
}

// 胜出候选确认后，落选候选独有的交易回到排队状态；共享交易保持已确认。
func TestCandidateLoserTxsReturnToPool(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 20, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 30, 100)
	for _, tx := range []*Transaction{a1, a2, b1} {
		n.Submit(tx)
	}
	local, _ := n.Propose() // [b1, a1, a2]
	// alt 只含 a1, b1（不含 a2）。
	alt, _ := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()})

	// 投 3 票给 alt 确认。
	for i := 0; i < 3; i++ {
		n.Vote(keys[i].pub, 1, alt.BlockID)
	}
	if n.CurrentRound() != 2 {
		t.Fatalf("round = %d, want 2", n.CurrentRound())
	}
	// a1, b1 已确认；a2 回到排队状态。
	for _, id := range []string{a1.ID(), b1.ID()} {
		info, _ := n.Tx(id)
		if info.Status != StatusConfirmed {
			t.Fatalf("tx %s status = %s, want confirmed", id, info.Status)
		}
	}
	a2Info, _ := n.Tx(a2.ID())
	if a2Info.Status != StatusQueued {
		t.Fatalf("a2 status = %s, want queued", a2Info.Status)
	}
	// 账户 A 已确认序号为 1（a1），a2 成为下一条可确认交易。
	if a := n.Account(keys[0].pub); a.ConfirmedSequence != 1 {
		t.Fatalf("A confirmed = %d, want 1", a.ConfirmedSequence)
	}
	// 下一轮 a2 可被打包。
	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.TxIDs) != 1 || p2.TxIDs[0] != a2.ID() {
		t.Fatalf("round 2 packed %v, want [a2]", p2.TxIDs)
	}
	_ = local
}

// 被候选引用的交易显示等待投票，禁止替换；仅池内等待的交易可替换。
func TestCandidateWaitingVoteAndReplacementLock(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 20, 100)
	n.Submit(a1)
	n.Submit(b1)
	n.Propose()
	// a1 被本地提议引用 → 等待投票，禁止替换。
	acct := n.Account(keys[0].pub)
	if len(acct.Pending) != 1 || acct.Pending[0].Note != "waiting-vote" {
		t.Fatalf("proposed tx should be waiting-vote: %+v", acct.Pending[0])
	}
	replace := NewTransaction(keys[0].priv, 1, []byte("a1-new"), 99, 100)
	if _, err := n.Submit(replace); reason(err) != ReasonProposalLocked {
		t.Fatalf("got %v, want %s", err, ReasonProposalLocked)
	}
	// 新提交的排队交易（未被候选引用）显示等待打包，可替换。
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), 5, 100)
	n.Submit(c1)
	acct = n.Account(keys[2].pub)
	if len(acct.Pending) != 1 || acct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("queued tx should be waiting-pack: %+v", acct.Pending[0])
	}
	c1Hi := NewTransaction(keys[2].priv, 1, []byte("c1-hi"), 99, 100)
	res, err := n.Submit(c1Hi)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReplacedID != c1.ID() {
		t.Fatal("queued tx should be replaceable")
	}
}

// EndRound 结束整轮：所有候选落选，交易回池，历史显示未确认结束。
func TestCandidateEndRound(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 3) // 进入轮次 3 时到期
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 20, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose()
	alt, _ := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()})
	// 投一票但不确认。
	n.Vote(keys[0].pub, 1, local.BlockID)

	round, err := n.EndRound()
	if err != nil {
		t.Fatal(err)
	}
	if round != 2 {
		t.Fatalf("round = %d, want 2", round)
	}
	// 无确认块。
	if n.Height() != 0 {
		t.Fatal("ended round must leave no block")
	}
	// 历史显示未确认结束，候选均落选。
	rc, err := n.RoundCandidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.Confirmed || rc.WonBlockID != "" {
		t.Fatalf("ended round view wrong: %+v", rc)
	}
	for _, c := range rc.Candidates {
		if c.Status != CandidateLost {
			t.Fatalf("candidate %s status = %s, want lost", c.BlockID, c.Status)
		}
	}
	// 投票者保留。
	for _, c := range rc.Candidates {
		if c.BlockID == local.BlockID && len(c.Voters) != 1 {
			t.Fatalf("voters not preserved in history: %+v", c.Voters)
		}
	}
	// 交易回到排队状态。
	for _, id := range []string{a1.ID(), b1.ID()} {
		info, _ := n.Tx(id)
		if info.Status != StatusQueued {
			t.Fatalf("tx %s status = %s, want queued", id, info.Status)
		}
	}
	_ = alt
}

// RoundCandidates 查询：当前轮与历史、未投票验证者、排序、深拷贝。
func TestRoundCandidatesQuery(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 20, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose()
	alt, _ := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()})
	n.Vote(keys[0].pub, 1, local.BlockID)

	rc, err := n.RoundCandidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Round != 1 || rc.Ended || rc.Confirmed {
		t.Fatalf("current round view wrong: %+v", rc)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(rc.Candidates))
	}
	// 按区块标识排序。
	if rc.Candidates[0].BlockID > rc.Candidates[1].BlockID {
		t.Fatal("candidates not sorted by block id")
	}
	// 未投票验证者为 keys[1], keys[2], keys[3]（按公钥排序）。
	if len(rc.NotVoted) != 3 {
		t.Fatalf("not-voted count = %d, want 3", len(rc.NotVoted))
	}
	// 深拷贝：修改返回结果不影响节点。
	rc.Candidates[0].TxIDs[0] = "tampered"
	rc.NotVoted[0][0] ^= 0xff
	rc2, _ := n.RoundCandidates(1)
	if rc2.Candidates[0].TxIDs[0] == "tampered" {
		t.Fatal("query result mutation affected node")
	}
	// 未产生候选的轮次查询：未来轮次报错。
	if _, err := n.RoundCandidates(5); reason(err) != ReasonUnknownRound {
		t.Fatalf("got %v, want %s", err, ReasonUnknownRound)
	}
	_ = alt
}

// 停止再启动恢复候选、每人已投的选择及历史结果。
func TestCandidatePersistenceRoundTrip(t *testing.T) {
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("persist-seed"), Validators: vals, MaxTxsPerBlock: 10})
	if err != nil {
		t.Fatal(err)
	}
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 20, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose()
	alt, _ := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()})
	n.Vote(keys[0].pub, 1, local.BlockID) // 未决一票

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n2.CurrentRound() != 1 {
		t.Fatal("round not restored")
	}
	rc, err := n2.RoundCandidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidates not restored: %+v", rc.Candidates)
	}
	// 本地候选与票数恢复。
	var localVoters [][]byte
	for _, c := range rc.Candidates {
		if c.BlockID == local.BlockID && c.Local {
			localVoters = c.Voters
		}
	}
	if len(localVoters) != 1 || string(localVoters[0]) != string(keys[0].pub) {
		t.Fatalf("local candidate/votes not restored: %+v", localVoters)
	}
	// 改投被拒（已投本地）。
	if _, err := n2.Vote(keys[0].pub, 1, alt.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("got %v, want %s", err, ReasonAlreadyVoted)
	}
	// 再补三票给 alt 确认（keys[0] 已投本地，故需其余三人）。
	n2.Vote(keys[1].pub, 1, alt.BlockID)
	n2.Vote(keys[2].pub, 1, alt.BlockID)
	res, err := n2.Vote(keys[3].pub, 1, alt.BlockID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Confirmed || res.Block.ID != alt.BlockID {
		t.Fatal("restored node should confirm alternative candidate")
	}
	// 历史结果恢复。
	rc2, _ := n2.RoundCandidates(1)
	if !rc2.Confirmed || rc2.WonBlockID != alt.BlockID {
		t.Fatalf("history not restored: %+v", rc2)
	}
	for _, c := range rc2.Candidates {
		if c.BlockID == alt.BlockID && c.Status != CandidateWon {
			t.Fatal("alt should be won")
		}
		if c.BlockID == local.BlockID && c.Status != CandidateLost {
			t.Fatal("local should be lost")
		}
	}
}

// 写入失败时登记/投票/结束不生效，状态保持操作前。
func TestCandidateAtomicityOnWriteFailure(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 20, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose()

	n.injectSaveErr = errors.New("disk full (simulated)")
	// 登记失败。
	if _, err := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()}); err == nil {
		t.Fatal("expected save error")
	}
	n.injectSaveErr = nil
	// 状态不变：只有本地候选。
	rc, _ := n.RoundCandidates(1)
	if len(rc.Candidates) != 1 {
		t.Fatalf("state changed after failed register: %+v", rc.Candidates)
	}

	// 登记成功后注入投票失败。
	alt, _ := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()})
	n.injectSaveErr = errors.New("disk full (simulated)")
	if _, err := n.Vote(keys[0].pub, 1, alt.BlockID); err == nil {
		t.Fatal("expected save error")
	}
	n.injectSaveErr = nil
	rc, _ = n.RoundCandidates(1)
	for _, c := range rc.Candidates {
		if c.BlockID == alt.BlockID && len(c.Voters) != 0 {
			t.Fatal("vote should not persist after failed save")
		}
	}
	_ = local
}

// v1 状态目录仍可打开：未决提议与票数作为本地候选恢复，旧轮次无候选详情。
func TestCandidateV1Migration(t *testing.T) {
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("migrate-seed"), Validators: vals, MaxTxsPerBlock: 10})
	if err != nil {
		t.Fatal(err)
	}
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	n.Submit(a1)
	p, _ := n.Propose()
	n.Vote(keys[0].pub, 1, p.BlockID)

	// 读取当前状态文件，改写为 v1 形态（proposal 字段，version 1）。
	path := filepath.Join(dir, stateFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["version"] = 1
	m["proposal"] = map[string]any{
		"round":    float64(1),
		"tx_ids":   []any{a1.ID()},
		"block_id": p.BlockID,
		"votes":    []any{fmt.Sprintf("%x", keys[0].pub)},
	}
	delete(m, "candidates")
	delete(m, "history")
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n2.CurrentRound() != 1 {
		t.Fatal("round not restored from v1")
	}
	// 未决提议作为本地候选恢复。
	rc, err := n2.RoundCandidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 1 || !rc.Candidates[0].Local || rc.Candidates[0].BlockID != p.BlockID {
		t.Fatalf("local candidate not migrated: %+v", rc.Candidates)
	}
	if len(rc.Candidates[0].Voters) != 1 {
		t.Fatalf("votes not migrated: %+v", rc.Candidates[0].Voters)
	}
	// 旧轮次无候选详情。
	if _, err := n2.RoundCandidates(0); reason(err) != ReasonUnknownRound {
		t.Fatalf("got %v, want %s", err, ReasonUnknownRound)
	}
	// 恢复后可正常投票确认。
	n2.Vote(keys[1].pub, 1, p.BlockID)
	res, err := n2.Vote(keys[2].pub, 1, p.BlockID)
	if err != nil || !res.Confirmed {
		t.Fatal("migrated node should confirm")
	}
}

// 空块候选：允许登记空块并按相同条件确认。
func TestCandidateEmptyBlock(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	n.Propose() // 本地空块
	local, _ := n.Proposal()
	// 登记空块候选 → 与本地同列表 → 返回本地候选。
	alt, err := n.RegisterCandidate(1, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID != local.BlockID {
		t.Fatal("empty candidate must match local empty proposal")
	}
	// 登记一个非空候选需要交易；这里仅验证空块确认流程。
	confirmByVotes(t, n, keys, local.BlockID)
	if n.Height() != 1 {
		t.Fatal("empty block should confirm")
	}
}
