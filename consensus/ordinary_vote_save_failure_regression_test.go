package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住“普通投票”保存失败的原子落盘回归保障。普通投票指即使成功计入，
// 该候选得票也仍然达不到确认门槛（四名验证者需严格超过 2/3，即至少 3 票）的
// 一票；它与决定性投票的区别在于：失败时根本不应走到确认流程，唯一要守住的是
// 失败的提交不能提前占用验证者在本轮的选择。
//
// 场景：四名验证者参与同一轮竞争，本地提议包含两笔不同账户的交易，一个已登记的
// 竞争候选只含其中一笔（两候选区块标识不同），两笔交易进入下一轮也不会到期。
// 先由两名验证者分别向两个候选各投一票：两边各一票，另外两名验证者尚未投票，
// 两个候选都保持未决。第三名验证者投给本地提议时恰遇状态保存失败：
//   - Vote 必须返回保存错误且没有任何成功结果（结果为 nil）；该错误必须作为
//     保存失败上报，不能被混同为“重复投票不增票”的正常返回，也不能被当作
//     “改投被拒绝”的 already-voted；
//   - 本地提议不能出现他的票，竞争候选原来的一票也不能丢失；
//   - 候选查询仍把他列为未投票者；轮次、确认高度、本地提议与确认历史维持
//     操作前结果；磁盘状态文件逐字节不变、不残留临时文件，重开节点同样看不到
//     他本轮已做过选择。
//
// 保存恢复正常后，这名验证者应能直接投给竞争候选：不需要先重复失败时投本地的
// 选择，也不应被当作改投而拒绝。这一票作为新票成功计入（Counted），返回
// 未确认且没有确认块。此后本地候选只有原来那名投票者，竞争候选为原来那名
// 投票者加上刚才失败过的这名验证者，仍未投票的只剩第四名验证者；竞争候选两票
// 仍不满足四人名单至少三票的确认条件，两个候选继续保持未决。
//
// 同一名验证者成功投给竞争候选后：再投该候选应成功返回但不增加票数；再投本地
// 提议则必须以已有的 already-voted 原因拒绝，竞争候选中的原票保留，两个候选的
// 投票者与未投票名单都不因这两次操作改变。本组测试不新增候选规则，也不改变
// 确认门槛与一人一轮一票的既有规则。

// assertSplitPending 断言第 1 轮在两个候选各自持有既得票数、尚未决出时的完整状态：
// 轮次 1、确认高度 0、无确认块且确认历史为空；本地提议标识与交易顺序不变；
// 两个候选都保持 pending，投票者分别恰为 localVoters 与 altVoters；本轮未投票
// 验证者恰为 unvoted；两笔交易仍被未决候选引用、等待投票且不带确认块关联。
func assertSplitPending(t *testing.T, n *Node, local ProposalView, altID string, localVoters, altVoters, unvoted [][]byte, a1, b1 *Transaction) {
	t.Helper()
	if n.CurrentRound() != 1 {
		t.Fatalf("current round = %d, want 1", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("no confirmed block may exist while the round stays undecided")
	}
	// 操作前本就没有确认历史：高度 1 必须查无区块，失败的投票不能生成它。
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block at height 1: %v, want %s (no confirmation history)", err, ReasonUnknownBlock)
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
		t.Fatalf("competitor must stay pending: %+v", gotAlt)
	}
	if fmt.Sprint(hexKeys(gotLocal.Voters)) != fmt.Sprint(hexKeys(localVoters)) {
		t.Fatalf("local voters = %x, want %x", gotLocal.Voters, localVoters)
	}
	if fmt.Sprint(hexKeys(gotAlt.Voters)) != fmt.Sprint(hexKeys(altVoters)) {
		t.Fatalf("competitor voters = %x, want %x", gotAlt.Voters, altVoters)
	}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(unvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, unvoted)
	}

	// 两个候选都未决出：它们引用的交易既不能提前确认，也不能回到排队。
	for _, tx := range []*Transaction{a1, b1} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed (waiting for votes)", tx.ID(), info.Status)
		}
		if info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("tx %s must not reference a confirmed block: height=%d block=%q",
				tx.ID(), info.BlockHeight, info.BlockID)
		}
	}
}

// TestOrdinaryVoteSaveFailureDoesNotPrecommitChoice 覆盖普通投票保存失败的完整
// 回归序列：失败整体回滚（保存错误、结果为空、不占用本轮选择）-> 内存/磁盘/
// 重开三处一致 -> 恢复后直接投另一候选作为新票计入且不确认 -> 重复投票不增票、
// 改投以 already-voted 被拒且不改变任何名单。
func TestOrdinaryVoteSaveFailureDoesNotPrecommitChoice(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)

	// 两笔不同账户、到期足够远的交易。费用 b1(5) 高于 a1(1)，本地提议固定为
	// [b1 a1] 两笔全含；竞争候选只含 a1，标识与交易顺序都不同于本地提议。
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
	alt, err := n.RegisterCandidate(1, []string{a1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID == local.BlockID {
		t.Fatal("setup: competitor block id must differ from local proposal")
	}

	// 两名验证者分别向两个候选各投一票：两边各一票，都达不到三人确认门槛。
	localVoter := keys[0].pub
	altVoter := keys[1].pub
	if res, err := n.Vote(localVoter, 1, local.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("vote for local proposal: %+v %v", res, err)
	}
	if res, err := n.Vote(altVoter, 1, alt.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("vote for competitor: %+v %v", res, err)
	}
	third := keys[2].pub
	unvotedTwo := [][]byte{keys[2].pub, keys[3].pub}
	localOne := [][]byte{keys[0].pub}
	altOne := [][]byte{keys[1].pub}
	assertSplitPending(t, n, local, alt.BlockID, localOne, altOne, unvotedTwo, a1, b1)

	// 快照失败前磁盘文件：失败的保存不得改写它，也不得残留临时文件。
	statePath := filepath.Join(dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 第三名验证者投给本地提议——这一票即使保存成功也只会让本地达到两票，
	// 仍达不到门槛；但此刻保存节点状态失败。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.Vote(third, 1, local.BlockID)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("ordinary vote must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as a save error, not duplicate-vote or already-voted %q: %v",
			reason(err), err)
	}
	if res != nil {
		t.Fatalf("vote result must be nil on save failure, got %+v", res)
	}

	// 失败提交不得提前占用他本轮的选择：本地没有他的票，竞争原票不丢，他仍是
	// 未投票者；轮次、确认高度、本地提议、交易状态与确认历史维持操作前结果。
	assertSplitPending(t, n, local, alt.BlockID, localOne, altOne, unvotedTwo, a1, b1)

	// 磁盘：状态文件逐字节不变，目录中仅有 state.json。
	assertStateFileUnchanged(t, dir, before)

	// 重开节点：保存记录里同样没有他本轮的选择，不能出现内存回滚、磁盘却已占用
	// 他投票资格的差异。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertSplitPending(t, reopened, local, alt.BlockID, localOne, altOne, unvotedTwo, a1, b1)

	// 保存恢复正常后，他可以直接投给竞争候选：不必先重复失败时投本地的选择，
	// 也不允许把这次投票当成“改投”而拒绝。
	res, err = reopened.Vote(third, 1, alt.BlockID)
	if err != nil {
		t.Fatalf("vote for competitor after recovery must be accepted as a fresh choice: %v", err)
	}
	if !res.Counted {
		t.Fatal("the vote must count as a new vote, not be deduped against the failed local one")
	}
	if res.Confirmed || res.Block != nil {
		t.Fatalf("two competitor votes must stay below the strict 3-of-4 threshold: %+v", res)
	}

	// 本地仅原投票者一人；竞争为原投票者加刚投票的第三人；只剩第四名未投票；
	// 两票仍不满足至少三票，两个候选都继续保持未决。
	altTwo := [][]byte{keys[1].pub, keys[2].pub}
	onlyFourth := [][]byte{keys[3].pub}
	assertSplitPending(t, reopened, local, alt.BlockID, localOne, altTwo, onlyFourth, a1, b1)

	// 同一名验证者再投竞争候选：成功返回但不增加票数、不确认。
	dup, err := reopened.Vote(third, 1, alt.BlockID)
	if err != nil {
		t.Fatalf("re-voting the same candidate must succeed without error: %v", err)
	}
	if dup.Counted || dup.Confirmed || dup.Block != nil {
		t.Fatalf("duplicate vote must not count or confirm: %+v", dup)
	}

	// 再投本地提议：按一人一轮一票规则以 already-voted 拒绝，竞争候选中的原票保留。
	if _, err := reopened.Vote(third, 1, local.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switching candidates after a counted vote got %v, want %s",
			err, ReasonAlreadyVoted)
	}

	// 重复投票与被拒改投都不改变两个候选的投票者与未投票名单。
	assertSplitPending(t, reopened, local, alt.BlockID, localOne, altTwo, onlyFourth, a1, b1)
}
