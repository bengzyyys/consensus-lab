package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住“主动结束未确认轮次”的原子落盘回归保障。
//
// 场景：四名验证者（确认门槛为票数严格超过名单的 2/3，即 3 票）。第 1 轮已有
// 一笔确认块（确立确认历史与账户已确认序号）；当前为第 2 轮，本地提议与一个不同
// 交易顺序的竞争候选均已登记，两者共享一笔交易，又各有一笔仅被自己引用的交易，
// 此外池中还有一笔从未被候选引用的排队交易。两名验证者分别投给两个候选，均未达
// 确认门槛。候选交易的到期轮次恰好为下一轮（3，结束即过期）或晚于下一轮（4，
// 结束后回到排队），未被引用的排队交易同样覆盖到期与未到期两种结果。
//
// 保存失败时，结束轮次必须整体不生效：返回轮次为 0 且报错，当前轮次不变；
// 本地提议标识与交易顺序、候选 pending 结果、已有投票与未投票名单照旧；等待投票
// 的交易不能提前回到排队，仍在排队的交易不能提前过期；账户的已确认序号、待处理
// 交易与缺口说明照旧；已确认区块的高度、标识与交易顺序不被改写；磁盘记录中不出现
// 本次结束造成的任何变化，重新打开节点看到同样未结束的状态。保存恢复正常后，在仍
// 处于原轮次的节点上再次结束才进入下一轮：旧轮明确显示未确认结束、两个候选均落选、
// 原有投票仍可查询；未到期的候选交易回到排队且共享交易只保留一份，到期轮次不大于
// 新轮次的交易（含此前未被候选引用者）从待处理列表移除并可查为过期；不增加确认块、
// 不推进账户已确认序号。本组测试不改变公开入口、候选规则与确认门槛。

// endRoundFixture 汇总一次测试所构造的交易与候选，便于失败前后与重开后复用断言。
type endRoundFixture struct {
	local           ProposalView
	altID           string
	history         Block // 第 1 轮确认块，结束第 2 轮不得改写
	shared          *Transaction
	localOnly       *Transaction
	altOnly         *Transaction
	queuedExpiring  *Transaction // 未被候选引用、到期轮次恰为下一轮（3）
	queuedSurviving *Transaction // 未被候选引用、到期轮次晚于下一轮（4）
}

// assertWaitingRoundTwo 断言节点仍停留在第 2 轮投票前/失败后的未决状态：
// 轮次 2、确认高度 1，历史块保持第 1 轮确认时的高度、标识与交易顺序；
// 本地提议标识与交易顺序不变；两个候选均为 pending，投票分别为给定名单，
// 未投票名单恰好为 unvoted；候选引用的三笔交易仍等待投票，不能提前回到排队；
// 未被引用的两笔交易仍在排队，到期轮次为 3 者不能提前过期；账户的已确认序号、
// 待处理交易与缺口说明保持操作前结果。
func assertWaitingRoundTwo(t *testing.T, n *Node, fx endRoundFixture, localVoters, altVoters, unvoted [][]byte, keys []testKey) {
	t.Helper()
	if n.CurrentRound() != 2 {
		t.Fatalf("current round = %d, want 2", n.CurrentRound())
	}
	if n.Height() != 1 {
		t.Fatalf("confirmed height = %d, want 1 (ending a round adds no block)", n.Height())
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.Height != fx.history.Height || blk.Round != fx.history.Round || blk.ID != fx.history.ID ||
		blk.PreviousID != fx.history.PreviousID || fmt.Sprint(blk.TxIDs) != fmt.Sprint(fx.history.TxIDs) {
		t.Fatalf("confirmed block rewritten by the round end: got %+v, want %+v", blk, fx.history)
	}
	if latest, ok := n.LatestBlock(); !ok || latest.ID != fx.history.ID ||
		fmt.Sprint(latest.TxIDs) != fmt.Sprint(fx.history.TxIDs) {
		t.Fatalf("latest block changed: %+v ok=%v, want %+v", latest, ok, fx.history)
	}

	p, ok := n.Proposal()
	if !ok {
		t.Fatal("local proposal must still be available in the pending round")
	}
	if p.Round != 2 || p.BlockID != fx.local.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(fx.local.TxIDs) {
		t.Fatalf("local proposal changed: round=%d id=%s txs=%v, want round=2 id=%s txs=%v",
			p.Round, p.BlockID, p.TxIDs, fx.local.BlockID, fx.local.TxIDs)
	}

	rc, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 2 must stay pending with records: %+v", rc)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2 (local + competitor)", len(rc.Candidates))
	}
	var gotLocal, gotAlt *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case fx.local.BlockID:
			gotLocal = &rc.Candidates[i]
		case fx.altID:
			gotAlt = &rc.Candidates[i]
		}
	}
	if gotLocal == nil || gotAlt == nil {
		t.Fatalf("both candidates must remain present: local=%p alt=%p views=%+v", gotLocal, gotAlt, rc.Candidates)
	}
	if !gotLocal.Local || gotLocal.Result != CandidatePending {
		t.Fatalf("local candidate must stay pending: %+v", gotLocal)
	}
	if fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint(fx.local.TxIDs) {
		t.Fatalf("local candidate tx order changed: %v, want %v", gotLocal.TxIDs, fx.local.TxIDs)
	}
	if fmt.Sprint(hexKeys(gotLocal.Voters)) != fmt.Sprint(hexKeys(localVoters)) {
		t.Fatalf("local candidate voters = %x, want %x", gotLocal.Voters, localVoters)
	}
	if gotAlt.Local || gotAlt.Result != CandidatePending {
		t.Fatalf("competitor must stay pending: %+v", gotAlt)
	}
	if fmt.Sprint(gotAlt.TxIDs) != fmt.Sprint([]string{fx.altOnly.ID(), fx.shared.ID()}) {
		t.Fatalf("competitor tx order changed: %v, want [altOnly shared]", gotAlt.TxIDs)
	}
	if fmt.Sprint(hexKeys(gotAlt.Voters)) != fmt.Sprint(hexKeys(altVoters)) {
		t.Fatalf("competitor voters = %x, want %x", gotAlt.Voters, altVoters)
	}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(unvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, unvoted)
	}

	// 被任一未决候选引用的交易不能提前回到排队：仍是 proposed、等待投票。
	for _, tx := range []*Transaction{fx.shared, fx.localOnly, fx.altOnly} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusProposed {
			t.Fatalf("candidate tx %s status = %s, want proposed (waiting for votes)", tx.ID(), info.Status)
		}
		if info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("candidate tx %s must not reference a confirmed block: height=%d block=%q",
				tx.ID(), info.BlockHeight, info.BlockID)
		}
	}
	// 未被候选引用的排队交易不能提前过期：仍是 queued、等待打包。
	for _, tx := range []*Transaction{fx.queuedExpiring, fx.queuedSurviving} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusQueued {
			t.Fatalf("queued tx %s status = %s, want queued (must not expire before the round advances)",
				tx.ID(), info.Status)
		}
	}

	// 账户视图与操作前一致：keys[0] 序号 1 已确认，序号 2（共享）等待投票；
	// keys[1]、keys[2] 的候选交易等待投票；keys[3] 的两笔排队交易等待打包且无缺口。
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{fx.shared.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{fx.localOnly.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{fx.altOnly.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[3].pub, 0, map[string]string{
		fx.queuedExpiring.ID():  "waiting-pack",
		fx.queuedSurviving.ID(): "waiting-pack",
	})
	if a := n.Account(keys[0].pub); a.Gap != 0 {
		t.Fatalf("keys[0] gap = %d, want 0 (seq 1 confirmed, seq 2 pending)", a.Gap)
	}
	if a := n.Account(keys[3].pub); a.Gap != 0 {
		t.Fatalf("keys[3] gap = %d, want 0", a.Gap)
	}
}

// setupEndRoundFixture 构造第 1 轮确认历史与第 2 轮未决候选场景，返回固定夹具。
func setupEndRoundFixture(t *testing.T) (*Node, endRoundFixture, []testKey) {
	t.Helper()
	keys := genKeys(t, 4)
	// 单块上限 3：本地提议恰好装下三笔候选交易，两笔排队交易都不被引用。
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("endround-save-seed"), Validators: vals, MaxTxsPerBlock: 3})
	if err != nil {
		t.Fatal(err)
	}

	// 第 1 轮：keys[0] 序号 1 的交易经本地提议确认，留下一笔确认块与已确认序号 1。
	first := NewTransaction(keys[0].priv, 1, []byte("round1"), 1, 100)
	if _, err := n.Submit(first); err != nil {
		t.Fatal(err)
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	confirmByVotes(t, n, keys, p1.BlockID)
	history, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}

	// 第 2 轮的候选交易（费用刻意拉开，使打包顺序确定，不依赖随机标识）：
	//   shared（keys[0] 序号 2，费用 100，到期 4）：两个候选共享，结束后未到期、
	//   回到排队；
	//   localOnly（keys[1] 序号 1，费用 90，到期 3）：仅本地提议引用，结束进入
	//   第 3 轮即过期；
	//   altOnly（keys[2] 序号 1，费用 80，到期 4）：仅竞争候选引用，结束后回到排队。
	shared := NewTransaction(keys[0].priv, 2, []byte("shared"), 100, 4)
	localOnly := NewTransaction(keys[1].priv, 1, []byte("local-only"), 90, 3)
	altOnly := NewTransaction(keys[2].priv, 1, []byte("alt-only"), 80, 4)
	// 未被任何候选引用的排队交易：一笔到期轮次恰为下一轮（3），一笔晚于下一轮（4）。
	// queuedExpiring 费用最低，三笔候选交易占满上限后它仍留在池中；queuedSurviving
	// 是 keys[3] 的序号 2，因序号 1 缺口在本轮打包时本就不可选。
	queuedExpiring := NewTransaction(keys[3].priv, 1, []byte("queued-expiring"), 10, 3)
	queuedSurviving := NewTransaction(keys[3].priv, 2, []byte("queued-surviving"), 5, 4)
	for _, tx := range []*Transaction{shared, localOnly, altOnly, queuedExpiring, queuedSurviving} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 费用决定打包顺序为 [shared, localOnly, altOnly]，两笔排队交易都不进提议。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{shared.ID(), localOnly.ID(), altOnly.ID()}) {
		t.Fatalf("setup: local proposal = %v, want [shared localOnly altOnly]", local.TxIDs)
	}
	// 竞争候选使用不同的交易顺序并以 altOnly 开头，区块标识必然不同。
	alt, err := n.RegisterCandidate(2, []string{altOnly.ID(), shared.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID == local.BlockID {
		t.Fatal("setup: competitor block id must differ from local proposal")
	}
	if alt.Existing {
		t.Fatal("setup: competitor must be a new candidate, not an existing one")
	}

	fx := endRoundFixture{
		local:           local,
		altID:           alt.BlockID,
		history:         history,
		shared:          shared,
		localOnly:       localOnly,
		altOnly:         altOnly,
		queuedExpiring:  queuedExpiring,
		queuedSurviving: queuedSurviving,
	}
	return n, fx, keys
}

// TestEndRoundSaveFailureAtomic 覆盖主动结束轮次保存失败的完整回归序列：
// 失败整体不生效 -> 内存/磁盘/重开三处一致 -> 恢复后在原轮次再次结束才进入下一轮。
func TestEndRoundSaveFailureAtomic(t *testing.T) {
	n, fx, keys := setupEndRoundFixture(t)

	// 两名验证者分别投给两个候选：各一票，均远未达到 3 票确认门槛。
	localVoters := [][]byte{keys[0].pub}
	altVoters := [][]byte{keys[1].pub}
	stillUnvoted := [][]byte{keys[2].pub, keys[3].pub}
	if res, err := n.Vote(keys[0].pub, 2, fx.local.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("local candidate vote: %+v %v", res, err)
	}
	if res, err := n.Vote(keys[1].pub, 2, fx.altID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("competitor vote: %+v %v", res, err)
	}
	assertWaitingRoundTwo(t, n, fx, localVoters, altVoters, stillUnvoted, keys)

	// 记录失败前的磁盘文件：后续失败的保存不得改动它。
	statePath := filepath.Join(n.dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 此刻保存节点状态失败：结束轮次必须报错且返回轮次为 0。
	n.injectSaveErr = errors.New("disk full (simulated)")
	r, err := n.EndRound()
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("EndRound must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if r != 0 {
		t.Fatalf("round returned on save failure = %d, want 0", r)
	}

	// 内存：轮次、历史块、本地提议、候选结果/投票/未投票名单、交易与账户全部照旧。
	assertWaitingRoundTwo(t, n, fx, localVoters, altVoters, stillUnvoted, keys)

	// 磁盘：状态文件逐字节保持操作前内容，且不残留临时文件。
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("state file changed despite the failed save")
	}
	entries, err := os.ReadDir(n.dir)
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

	// 重开节点：看到同样的未结束状态——轮次没有前进，候选没有落选，
	// 等待投票的交易没有回到排队，排队交易没有提前过期。
	reopened, err := Open(n.dir)
	if err != nil {
		t.Fatal(err)
	}
	assertWaitingRoundTwo(t, reopened, fx, localVoters, altVoters, stillUnvoted, keys)

	// 保存恢复正常后，在仍处于原轮次（第 2 轮）的节点上再次结束才进入下一轮。
	r, err = reopened.EndRound()
	if err != nil {
		t.Fatalf("EndRound after recovery: %v", err)
	}
	if r != 3 || reopened.CurrentRound() != 3 {
		t.Fatalf("round after recovered end = %d (current %d), want 3", r, reopened.CurrentRound())
	}
	assertAfterEndRoundOutcomes(t, reopened, fx, localVoters, altVoters, stillUnvoted, keys)

	// 结束结果随再次落盘持久化：再开一个节点看到完全一致的已结束状态。
	durable, err := Open(n.dir)
	if err != nil {
		t.Fatal(err)
	}
	if durable.CurrentRound() != 3 {
		t.Fatalf("durable current round = %d, want 3", durable.CurrentRound())
	}
	assertAfterEndRoundOutcomes(t, durable, fx, localVoters, altVoters, stillUnvoted, keys)
}

// assertAfterEndRoundOutcomes 断言保存恢复后再次结束的既有结果：
// 旧轮明确显示未确认结束、两个候选均落选且原有投票仍可查询；未到期的候选交易
// 回到排队（共享交易只保留一份），到期轮次不大于新轮次（3）的交易从待处理列表
// 移除并可查为过期，包括此前没有被候选引用的交易；不增加确认块、不推进已确认序号。
func assertAfterEndRoundOutcomes(t *testing.T, n *Node, fx endRoundFixture, localVoters, altVoters, unvoted [][]byte, keys []testKey) {
	t.Helper()
	if n.Height() != 1 {
		t.Fatalf("height after EndRound = %d, want 1 (no confirmed block added)", n.Height())
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != fx.history.ID || fmt.Sprint(blk.TxIDs) != fmt.Sprint(fx.history.TxIDs) {
		t.Fatalf("historical block changed after EndRound: %+v", blk)
	}

	// 当前轮次（3）尚未产生本地提议；旧轮（2）明确显示未确认结束。
	if _, ok := n.Proposal(); ok {
		t.Fatal("round 3 must not carry over the ended round's proposal")
	}
	rc, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || !rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 2 must be recorded as an unconfirmed end with records: %+v", rc)
	}
	var gotLocal, gotAlt *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case fx.local.BlockID:
			gotLocal = &rc.Candidates[i]
		case fx.altID:
			gotAlt = &rc.Candidates[i]
		}
	}
	if gotLocal == nil || gotAlt == nil {
		t.Fatalf("both candidates must remain in history: %+v", rc.Candidates)
	}
	// 两个候选均落选，标识与交易顺序保持原样，原有投票仍可查询。
	for _, c := range []*CandidateView{gotLocal, gotAlt} {
		if c.Result != CandidateLost {
			t.Fatalf("candidate %s result = %s, want lost", c.BlockID, c.Result)
		}
	}
	if fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint(fx.local.TxIDs) {
		t.Fatalf("local candidate tx order changed in history: %v, want %v", gotLocal.TxIDs, fx.local.TxIDs)
	}
	if fmt.Sprint(gotAlt.TxIDs) != fmt.Sprint([]string{fx.altOnly.ID(), fx.shared.ID()}) {
		t.Fatalf("competitor tx order changed in history: %v, want [altOnly shared]", gotAlt.TxIDs)
	}
	if fmt.Sprint(hexKeys(gotLocal.Voters)) != fmt.Sprint(hexKeys(localVoters)) {
		t.Fatalf("local candidate votes lost after end: %x, want %x", gotLocal.Voters, localVoters)
	}
	if fmt.Sprint(hexKeys(gotAlt.Voters)) != fmt.Sprint(hexKeys(altVoters)) {
		t.Fatalf("competitor votes lost after end: %x, want %x", gotAlt.Voters, altVoters)
	}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(unvoted)) {
		t.Fatalf("old-round unvoted = %x, want %x", rc.Unvoted, unvoted)
	}

	// 未到期（到期轮次 4 > 新轮次 3）的候选交易回到排队；共享交易被两个落选候选
	// 引用也只保留一份，状态只翻转一次。
	for _, tx := range []*Transaction{fx.shared, fx.altOnly} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusQueued {
			t.Fatalf("surviving candidate tx %s status = %s, want queued", tx.ID(), info.Status)
		}
		if info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("returned tx %s must not reference a block: %+v", tx.ID(), info)
		}
	}
	// 到期轮次不大于新轮次（3）的交易失效：候选独有的 localOnly 与未被引用的
	// queuedExpiring 均可查为过期，并已从待处理列表移除。
	for _, tx := range []*Transaction{fx.localOnly, fx.queuedExpiring} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusExpired {
			t.Fatalf("expiring tx %s status = %s, want expired", tx.ID(), info.Status)
		}
		if info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("expired tx %s must not reference a block: %+v", tx.ID(), info)
		}
	}
	// 未被候选引用、到期更晚的排队交易原样留在队列。
	if info := n.mustTx(t, fx.queuedSurviving.ID()); info.Status != StatusQueued {
		t.Fatalf("queued surviving tx status = %s, want queued", info.Status)
	}

	// 账户视图：已确认序号不推进；待处理列表只含回到排队或仍在排队的未到期交易，
	// 过期交易不再出现；keys[0] 的序号 2 与已确认序号 1 相邻，无缺口。
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{fx.shared.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{fx.altOnly.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[3].pub, 0, map[string]string{fx.queuedSurviving.ID(): "waiting-pack"})
	if a := n.Account(keys[0].pub); a.Gap != 0 {
		t.Fatalf("keys[0] gap = %d, want 0", a.Gap)
	}
	if a := n.Account(keys[1].pub); a.Gap != 0 {
		// 序号 1 的交易已过期：待处理列表为空，不存在“等待中的缺口”。
		t.Fatalf("keys[1] gap = %d, want 0 with an empty pending list", a.Gap)
	}
	if a := n.Account(keys[3].pub); a.Gap != 1 {
		// 序号 1 已过期移除，序号 2 仍在排队：最早缺口说明为序号 1。
		t.Fatalf("keys[3] gap = %d, want 1 (seq 1 expired, seq 2 queued)", a.Gap)
	}
}
