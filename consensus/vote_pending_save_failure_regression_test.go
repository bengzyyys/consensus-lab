package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住“普通投票保存失败”的回归保障：普通票指即使成功计入也不会达到
// 确认门槛的一票。重点是失败的提交绝不能提前占用该验证者在本轮的选择——
// 保存错误必须作为保存失败（基础设施错误）返回，不能被混同为重复投票去重或
// 改投被拒绝；已有的确认门槛与“一轮只能选择一个候选”规则继续适用。
//
// 场景：四名验证者（确认门槛为票数严格超过名单的 2/3，即 3 票）参与同一轮
// 竞争。本地提议包含两笔不同账户的交易，已登记的竞争候选只含其中一笔，两者
// 区块标识不同。验证者 0 投本地提议、验证者 1 投竞争候选后，两边各持一票，
// 验证者 2、3 尚未投票，两个候选都保持未决。验证者 2 改投本地提议时保存
// 失败：调用必须返回错误且没有成功结果，本地提议不出现他的票，竞争候选原有
// 的一票也不能丢失，轮次查询仍把他列为未投票者，轮次、确认高度与确认历史
// 维持操作前结果；重开节点看到同样的状态。保存恢复正常后，他不需要先重复
// 失败时的选择，可以直接投给竞争候选并作为新票计入（已计票、未确认、无确认
// 块）；竞争候选两票仍不满足四人名单至少三票的门槛。此后他再投竞争候选应
// 成功返回但不增票，再投本地提议则以 already-voted 被拒绝，两次操作都不改变
// 两个候选的投票者与未投票名单。

// assertRoundOneSplitVotes 断言第 1 轮维持操作前的未决状态：
// 轮次 1、确认高度 0、不存在确认块与确认历史；本地提议标识与交易顺序不变；
// 本地候选与竞争候选均为 pending，投票者恰好为给定集合，未投票名单恰好为
// unvoted；两笔交易仍处于等待投票状态。
func assertRoundOneSplitVotes(t *testing.T, n *Node, local ProposalView, altID string, localVoters, altVoters, unvoted [][]byte, a1, b1 *Transaction) {
	t.Helper()
	if n.CurrentRound() != 1 {
		t.Fatalf("current round = %d, want 1", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("latest confirmed block must not exist while the round is pending")
	}
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("confirmation history must stay empty, BlockAt(1) got %v", err)
	}

	p, ok := n.Proposal()
	if !ok {
		t.Fatal("local proposal must still be available")
	}
	if p.BlockID != local.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(local.TxIDs) {
		t.Fatalf("local proposal changed: id=%s txs=%v, want id=%s txs=%v",
			p.BlockID, p.TxIDs, local.BlockID, local.TxIDs)
	}

	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 1 must stay pending with records: %+v", rc)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2 (local + competitor)", len(rc.Candidates))
	}
	var gotLocal, gotAlt *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case local.BlockID:
			gotLocal = &rc.Candidates[i]
		case altID:
			gotAlt = &rc.Candidates[i]
		}
	}
	if gotLocal == nil || gotAlt == nil {
		t.Fatalf("both candidates must remain present: local=%p alt=%p views=%+v", gotLocal, gotAlt, rc.Candidates)
	}
	if !gotLocal.Local || gotLocal.Result != CandidatePending {
		t.Fatalf("local candidate must stay pending: %+v", gotLocal)
	}
	if fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint(local.TxIDs) {
		t.Fatalf("local candidate tx order changed: %v, want %v", gotLocal.TxIDs, local.TxIDs)
	}
	if gotAlt.Local || gotAlt.Result != CandidatePending {
		t.Fatalf("competitor must be non-local and pending: %+v", gotAlt)
	}
	if fmt.Sprint(gotAlt.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("competitor tx order = %v, want [%s]", gotAlt.TxIDs, a1.ID())
	}
	if fmt.Sprint(hexKeys(gotLocal.Voters)) != fmt.Sprint(hexKeys(localVoters)) {
		t.Fatalf("local candidate voters = %x, want %x", gotLocal.Voters, localVoters)
	}
	if fmt.Sprint(hexKeys(gotAlt.Voters)) != fmt.Sprint(hexKeys(altVoters)) {
		t.Fatalf("competitor voters = %x, want %x", gotAlt.Voters, altVoters)
	}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(unvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, unvoted)
	}

	// 两笔交易都不能提前确认，仍被未决候选引用、等待投票。
	for _, tx := range []*Transaction{a1, b1} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed (waiting for votes)", tx.ID(), info.Status)
		}
		if info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("tx %s must not reference a confirmed block: height=%d block=%q", tx.ID(), info.BlockHeight, info.BlockID)
		}
	}
}

// TestPendingVoteSaveFailureLeavesValidatorChoiceFree 覆盖普通投票保存失败的
// 完整回归序列：失败整体不生效且不占用选择 -> 恢复后可直接投另一个候选并作为
// 新票计入 -> 之后的重复投票去重与改投拒绝行为不受失败操作影响。
func TestPendingVoteSaveFailureLeavesValidatorChoiceFree(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)

	// 两笔不同账户的交易，费用 b1(5) 高于 a1(1)：本地提议顺序固定为 b1、a1。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	if _, err := n.Submit(a1); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(b1); err != nil {
		t.Fatal(err)
	}
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{b1.ID(), a1.ID()}) {
		t.Fatalf("setup: local proposal = %v, want [b1 a1]", local.TxIDs)
	}
	// 竞争候选只含 a1，区块标识与本地提议不同。
	alt, err := n.RegisterCandidate(1, []string{a1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID == local.BlockID {
		t.Fatal("setup: competitor block id must differ from local proposal")
	}

	// 两名验证者分别投向两个候选：两边各一票，均远未达到 3 票门槛。
	v0, err := n.Vote(keys[0].pub, 1, local.BlockID)
	if err != nil || !v0.Counted || v0.Confirmed || v0.Block != nil {
		t.Fatalf("validator 0 vote for local proposal: %+v %v", v0, err)
	}
	v1, err := n.Vote(keys[1].pub, 1, alt.BlockID)
	if err != nil || !v1.Counted || v1.Confirmed || v1.Block != nil {
		t.Fatalf("validator 1 vote for competitor: %+v %v", v1, err)
	}

	// 操作前状态：两个候选各持一票、保持未决，验证者 2、3 尚未投票。
	preLocalVoters := [][]byte{keys[0].pub}
	preAltVoters := [][]byte{keys[1].pub}
	preUnvoted := [][]byte{keys[2].pub, keys[3].pub}
	assertRoundOneSplitVotes(t, n, local, alt.BlockID, preLocalVoters, preAltVoters, preUnvoted, a1, b1)

	// 记录失败投票前的磁盘文件：失败的保存不得改动它。
	statePath := filepath.Join(dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 验证者 2 的一票即使成功也只会让本地提议达到两票，仍不满足门槛；
	// 但这一次状态保存失败。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.Vote(keys[2].pub, 1, local.BlockID)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("the vote must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not duplicate/switch rejection %q: %v", reason(err), err)
	}
	if res != nil {
		t.Fatalf("vote result must be nil on save failure, got %+v", res)
	}

	// 内存：失败的一票没有出现在本地提议下，竞争候选原票保留，验证者 2 仍未投票；
	// 轮次、确认高度与确认历史全部维持操作前结果。
	assertRoundOneSplitVotes(t, n, local, alt.BlockID, preLocalVoters, preAltVoters, preUnvoted, a1, b1)

	// 磁盘：状态文件逐字节保持操作前内容，且不残留临时文件。
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("state file changed despite the failed save")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[state.json]" {
		t.Fatalf("failed save must not leave temp files, dir contains %v", names)
	}

	// 重开节点：失败的一票同样没有留在磁盘状态里，验证者 2 依旧未投票。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRoundOneSplitVotes(t, reopened, local, alt.BlockID, preLocalVoters, preAltVoters, preUnvoted, a1, b1)

	// 保存恢复正常：验证者 2 不需要先重复失败时投本地提议的选择，可直接投给
	// 竞争候选。该票必须作为新票计入（Counted），而不是被当作重复投票去重；
	// 两票仍不满足四人名单至少三票的门槛，故未确认且没有确认块。
	res, err = reopened.Vote(keys[2].pub, 1, alt.BlockID)
	if err != nil {
		t.Fatalf("vote for competitor after recovery: %v", err)
	}
	if !res.Counted {
		t.Fatal("the recovered vote must count as a new vote, not dedup as the failed local-proposal choice")
	}
	if res.Confirmed || res.Block != nil {
		t.Fatalf("two competitor votes must stay below the 3-of-4 threshold: %+v", res)
	}

	// 投票归属：本地提议只剩验证者 0；竞争候选为验证者 1 与刚投票的验证者 2；
	// 未投票者只剩验证者 3。两个候选都继续保持未决。
	postLocalVoters := [][]byte{keys[0].pub}
	postAltVoters := [][]byte{keys[1].pub, keys[2].pub}
	postUnvoted := [][]byte{keys[3].pub}
	assertRoundOneSplitVotes(t, reopened, local, alt.BlockID, postLocalVoters, postAltVoters, postUnvoted, a1, b1)

	// 验证者 2 再投同一竞争候选：成功返回但不增加票数。
	dup, err := reopened.Vote(keys[2].pub, 1, alt.BlockID)
	if err != nil {
		t.Fatalf("duplicate vote for the same candidate must not error: %v", err)
	}
	if dup.Counted || dup.Confirmed || dup.Block != nil {
		t.Fatalf("duplicate vote must be acknowledged without counting: %+v", dup)
	}
	assertRoundOneSplitVotes(t, reopened, local, alt.BlockID, postLocalVoters, postAltVoters, postUnvoted, a1, b1)

	// 验证者 2 再投本地提议：他本轮的有效选择是竞争候选，必须按改投拒绝；
	// 竞争候选中的原票保留，两个候选的投票者与未投票名单都不变。
	if _, err := reopened.Vote(keys[2].pub, 1, local.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switching to local proposal got %v, want %s", err, ReasonAlreadyVoted)
	}
	assertRoundOneSplitVotes(t, reopened, local, alt.BlockID, postLocalVoters, postAltVoters, postUnvoted, a1, b1)

	// 上述结果随恢复后的落盘持久化：再次重开，投票归属与未决状态完全一致。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRoundOneSplitVotes(t, durable, local, alt.BlockID, postLocalVoters, postAltVoters, postUnvoted, a1, b1)
}
