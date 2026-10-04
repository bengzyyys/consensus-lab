package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// 本文件钉住“决定性投票”的原子落盘回归保障。
//
// 场景：四名验证者（确认门槛为票数严格超过名单的 2/3，即 3 票）的一轮竞争中，
// 本地提议包含两笔不同账户的交易，竞争候选只包含其中一笔，两笔交易进入下一轮
// 也不会到期。竞争候选已有两票（不足以确认），第三名验证者的一票本应使其胜出，
// 但这一次保存节点状态失败。该投票必须整体不生效：调用返回错误且结果为空，
// 当前轮次、确认高度、最新确认块、候选票数与结果、本地提议、两笔交易状态以及
// 账户已确认序号全部保持投票前状态；磁盘上同样不能留下第三票或临时文件。
// 重新打开节点看到的仍是同一份操作前状态。保存恢复正常后，同一名验证者对同一
// 竞争候选的再次投票必须作为新票被接收、立即确认，按候选标识与交易顺序生成
// 确认块并进入下一轮；落选本地提议独有的交易回到排队且不推进序号。
// 本组测试不新增候选规则，也不改变确认门槛。

// hexKeys 把验证者公钥转为十六进制并按字典序排列，便于与查询返回的排序结果比对。
func hexKeys(pubs [][]byte) []string {
	out := make([]string, 0, len(pubs))
	for _, p := range pubs {
		out = append(out, fmt.Sprintf("%x", p))
	}
	sort.Strings(out)
	return out
}

// assertRoundOnePending 断言节点正处于测试轮次（第 1 轮）投票前/失败后的未决状态：
// 轮次 1、高度 0、无确认块；本地提议标识与交易顺序不变；竞争候选保持给定票数与
// pending 结果、本地候选 0 票；未投票名单恰好为 unvoted；两笔交易仍在等待投票；
// 两个账户的已确认序号都为 0，待处理交易各一笔且说明为 waiting-vote。
func assertRoundOnePending(t *testing.T, n *Node, local ProposalView, altID string, altVoters, unvoted [][]byte, a1, b1 *Transaction, keys []testKey) {
	t.Helper()
	if n.CurrentRound() != 1 {
		t.Fatalf("current round = %d, want 1", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("latest confirmed block must not exist before confirmation")
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
	if !gotLocal.Local || gotLocal.Result != CandidatePending || len(gotLocal.Voters) != 0 {
		t.Fatalf("local candidate must stay pending with no votes: %+v", gotLocal)
	}
	if fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint(local.TxIDs) {
		t.Fatalf("local candidate tx order changed: %v, want %v", gotLocal.TxIDs, local.TxIDs)
	}
	if gotAlt.Local || gotAlt.Result != CandidatePending {
		t.Fatalf("competitor must stay pending: %+v", gotAlt)
	}
	if fmt.Sprint(gotAlt.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("competitor tx order = %v, want [%s]", gotAlt.TxIDs, a1.ID())
	}
	if fmt.Sprint(hexKeys(gotAlt.Voters)) != fmt.Sprint(hexKeys(altVoters)) {
		t.Fatalf("competitor voters = %x, want %x", gotAlt.Voters, altVoters)
	}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(unvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, unvoted)
	}

	// 两笔交易都不能提前确认，也不能回到排队：仍被未决候选引用、等待投票。
	for _, tx := range []*Transaction{a1, b1} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed (waiting for votes)", tx.ID(), info.Status)
		}
		if info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("tx %s must not reference a confirmed block: height=%d block=%q", tx.ID(), info.BlockHeight, info.BlockID)
		}
	}
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{a1.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{b1.ID(): "waiting-vote"})
	if a := n.Account(keys[0].pub); a.Gap != 0 {
		t.Fatalf("a1 account gap = %d, want 0", a.Gap)
	}
	if a := n.Account(keys[1].pub); a.Gap != 0 {
		t.Fatalf("b1 account gap = %d, want 0", a.Gap)
	}
}

// TestDecisiveVoteSaveFailureAtomic 覆盖决定性投票保存失败的完整回归序列：
// 失败整体回滚 -> 内存/磁盘/重开三处一致 -> 恢复后再次投票作为新票确认。
func TestDecisiveVoteSaveFailureAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)

	// 两笔不同账户的交易，到期轮次足够大：进入第 2 轮也不会失效。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	if _, err := n.Submit(a1); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(b1); err != nil {
		t.Fatal(err)
	}
	// 费用 b1(5) 高于 a1(1)：本地提议顺序固定为 b1、a1，两笔都在提议中。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{b1.ID(), a1.ID()}) {
		t.Fatalf("setup: local proposal = %v, want [b1 a1]", local.TxIDs)
	}
	// 竞争候选只含 a1，标识与交易顺序都不同于本地提议。
	alt, err := n.RegisterCandidate(1, []string{a1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID == local.BlockID {
		t.Fatal("setup: competitor block id must differ from local proposal")
	}

	// 两名验证者投给竞争候选：两票未达严格超过 2/3 的门槛，不能确认。
	firstVoters := [][]byte{keys[0].pub, keys[1].pub}
	for i, v := range firstVoters {
		res, err := n.Vote(v, 1, alt.BlockID)
		if err != nil || !res.Counted || res.Confirmed {
			t.Fatalf("competitor vote %d: %+v %v", i+1, res, err)
		}
	}
	decisive := keys[2].pub
	stillUnvoted := [][]byte{keys[2].pub, keys[3].pub}
	assertRoundOnePending(t, n, local, alt.BlockID, firstVoters, stillUnvoted, a1, b1, keys)

	// 记录决定性投票前的磁盘文件：后续失败的保存不得改动它。
	statePath := filepath.Join(dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 第三名验证者本应使竞争候选胜出，但此刻保存节点状态失败。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.Vote(decisive, 1, alt.BlockID)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("decisive vote must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a vote rejection %q: %v", reason(err), err)
	}
	if res != nil {
		t.Fatalf("vote result must be nil on save failure, got %+v", res)
	}

	// 内存：轮次、高度、最新块、候选票数/结果、未投票者、提议、交易与账户全部照旧。
	assertRoundOnePending(t, n, local, alt.BlockID, firstVoters, stillUnvoted, a1, b1, keys)

	// 磁盘：状态文件逐字节保持投票前内容，且不残留临时文件。
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

	// 重开节点：看到同样的操作前状态——不多出确认块，也没有只落在内存或
	// 只落在保存记录里的第三票。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRoundOnePending(t, reopened, local, alt.BlockID, firstVoters, stillUnvoted, a1, b1, keys)

	// 保存恢复正常后，同一名验证者再次投给同一竞争候选：
	// 必须作为新票（Counted）被接收并立即确认，而非被当成重复投票去重。
	res, err = reopened.Vote(decisive, 1, alt.BlockID)
	if err != nil {
		t.Fatalf("decisive vote after recovery: %v", err)
	}
	if !res.Counted {
		t.Fatal("the repeated vote must count as a new vote, not dedup as an earlier failed one")
	}
	if !res.Confirmed || res.Block == nil {
		t.Fatalf("competitor must confirm immediately on the third durable vote: %+v", res)
	}
	if res.Block.ID != alt.BlockID || fmt.Sprint(res.Block.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("returned block must match competitor id and tx order: %+v", res.Block)
	}
	if res.Block.Height != 1 || res.Block.Round != 1 || res.Block.PreviousID != "" {
		t.Fatalf("returned block header wrong: %+v", res.Block)
	}

	// 节点进入下一轮，确认高度推进，最新块即竞争候选块。
	if reopened.CurrentRound() != 2 || reopened.Height() != 1 {
		t.Fatalf("round=%d height=%d after confirmation, want 2/1", reopened.CurrentRound(), reopened.Height())
	}
	blk, err := reopened.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != alt.BlockID || blk.Round != 1 || blk.Height != 1 || blk.PreviousID != "" ||
		fmt.Sprint(blk.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("confirmed block header/order wrong: %+v", blk)
	}
	if latest, ok := reopened.LatestBlock(); !ok || latest.ID != alt.BlockID {
		t.Fatalf("latest block = %+v ok=%v, want competitor block", latest, ok)
	}

	// 竞争候选中的交易关联确认块，所属账户已确认序号推进到 1。
	winInfo := reopened.mustTx(t, a1.ID())
	if winInfo.Status != StatusConfirmed || winInfo.BlockID != alt.BlockID || winInfo.BlockHeight != 1 {
		t.Fatalf("winner tx wrong: %+v", winInfo)
	}
	if a := reopened.Account(keys[0].pub); a.ConfirmedSequence != 1 || len(a.Pending) != 0 {
		t.Fatalf("winner account wrong after confirmation: %+v", a)
	}
	// 本地提议独有的落选交易回到排队，账户序号不因落选改变；进入第 2 轮未到期。
	loseInfo := reopened.mustTx(t, b1.ID())
	if loseInfo.Status != StatusQueued {
		t.Fatalf("loser-only tx status = %s, want queued", loseInfo.Status)
	}
	if loseInfo.BlockHeight != 0 || loseInfo.BlockID != "" {
		t.Fatalf("loser-only tx must not reference a block: %+v", loseInfo)
	}
	assertAccountQueue(t, reopened, keys[1].pub, 0, map[string]string{b1.ID(): "waiting-pack"})

	// 旧轮记录：竞争候选胜出、本地提议落选，三票完整保留，第四名仍未投票。
	rc, err := reopened.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 1 must be a confirmed, recorded round: %+v", rc)
	}
	for _, c := range rc.Candidates {
		want := CandidateLost
		if c.BlockID == alt.BlockID {
			want = CandidateWon
			if len(c.Voters) != 3 {
				t.Fatalf("winner must keep exactly 3 votes including the recovered decisive one, got %d", len(c.Voters))
			}
		}
		if c.Result != want {
			t.Fatalf("candidate %s result = %s, want %s", c.BlockID, c.Result, want)
		}
	}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys([][]byte{keys[3].pub})) {
		t.Fatalf("old-round unvoted = %x, want only validator 3", rc.Unvoted)
	}

	// 确认结果随再次落盘持久化：再次重开后状态完全一致，不存在半生效的第三票。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if durable.CurrentRound() != 2 || durable.Height() != 1 {
		t.Fatalf("reopened round=%d height=%d, want 2/1", durable.CurrentRound(), durable.Height())
	}
	if b, ok := durable.LatestBlock(); !ok || b.ID != alt.BlockID || fmt.Sprint(b.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("durable latest block wrong: %+v ok=%v", b, ok)
	}
	if info := durable.mustTx(t, a1.ID()); info.Status != StatusConfirmed || info.BlockHeight != 1 {
		t.Fatalf("durable winner tx wrong: %+v", info)
	}
	if info := durable.mustTx(t, b1.ID()); info.Status != StatusQueued {
		t.Fatalf("durable loser tx wrong: %+v", info)
	}
}
