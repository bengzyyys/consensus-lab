package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住“首次生成本地提议”的原子落盘回归保障。
//
// 场景：节点已有确认历史（第 1 轮确认，当前处于第 2 轮）。第 2 轮池内同时有
// 能够连续打包的交易（账户0 的 seq2/seq3 接在已确认序号 1 之后，账户1 的 seq1）
// 和因账户序号缺口而留在池中的交易（账户2 缺 seq1、只有 seq2）。本轮还没有本地
// 提议时，Propose 会选择交易、建立候选并把选中的交易置为等待投票，只有保存成功
// 才对外生效。当这次 Propose 因节点状态写入失败而返回错误时：不提供可用的提议
// 结果；Proposal 仍表示本轮尚无本地提议，Candidates 查询本轮仍按既有规则返回
// unknown-round；当前轮次与确认高度不前进，原有确认块的标识与交易顺序不变；
// 全部交易查询仍显示 queued，账户待处理列表、已确认序号与最早缺口不变，等待
// 说明仍是 waiting-pack，不出现 waiting-vote。磁盘状态文件逐字节不变、不残留
// 临时文件，重开节点看到同样的操作前状态。
//
// 失败不能留下候选引用造成的限制：保存恢复正常后，对原本可替换的排队交易提交
// 同发送者、同序号且费用严格更高的新交易，仍按原有规则成功替换，不能以
// tx-in-proposal 拒绝。再生成本地提议时以此时池中的有效交易为准，采用替换后的
// 交易标识，并继续遵守账户序号连续、费用排序和单块上限，不复用失败调用中准备
// 过的旧列表。成功生成后才恢复既有的冻结行为：提议中的交易禁止替换，后续提交
// 不改写这份提议。
//
// 没有可打包交易、首次尝试生成空块时遵守同一规则：失败不登记空块候选，保存
// 恢复后才可生成正常的空块提议。本组测试不新增公开入口，也不改变现有拒绝原因
// 与正常提议结果。

// assertProposeRolledBack 断言首次 Propose 保存失败后节点完全处于操作前状态：
// 轮次 2、确认高度 1，确认块标识与交易顺序不变；本轮无本地提议、候选查询返回
// unknown-round；四笔池内交易全部 queued，账户已确认序号、待处理列表、最早
// 缺口与 waiting-pack 说明全部保持操作前结果，没有任何交易被置为等待投票。
func assertProposeRolledBack(t *testing.T, n *Node, confirmedBlock Block, keys []testKey, old, next, b1, gapTx *Transaction) {
	t.Helper()
	if n.CurrentRound() != 2 {
		t.Fatalf("current round = %d, want 2 (failed propose must not advance the round)", n.CurrentRound())
	}
	if n.Height() != 1 {
		t.Fatalf("confirmed height = %d, want 1", n.Height())
	}
	latest, ok := n.LatestBlock()
	if !ok || latest.ID != confirmedBlock.ID || fmt.Sprint(latest.TxIDs) != fmt.Sprint(confirmedBlock.TxIDs) {
		t.Fatalf("latest confirmed block changed: %+v ok=%v, want id=%s txs=%v",
			latest, ok, confirmedBlock.ID, confirmedBlock.TxIDs)
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != confirmedBlock.ID || blk.Round != confirmedBlock.Round ||
		fmt.Sprint(blk.TxIDs) != fmt.Sprint(confirmedBlock.TxIDs) {
		t.Fatalf("confirmed history changed:\n got=%+v\nwant=%+v", blk, confirmedBlock)
	}

	// 本轮尚无本地提议：Proposal 不可用，候选查询按既有规则返回 unknown-round。
	if p, ok := n.Proposal(); ok {
		t.Fatalf("round 2 must have no local proposal after the failed propose, got %+v", p)
	}
	if _, err := n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("candidates for round 2 must be %s, got %v", ReasonUnknownRound, err)
	}
	// 第 1 轮历史记录保留：已确认结束、唯一候选胜出。
	rc1, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc1.Ended || rc1.UnconfirmedEnd || !rc1.HasRecords {
		t.Fatalf("round 1 history must stay a confirmed, recorded round: %+v", rc1)
	}

	// 没有交易因失败的提议被锁住：全部仍是 queued，不带确认块关联。
	for _, tx := range []*Transaction{old, next, b1, gapTx} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusQueued {
			t.Fatalf("tx %q status = %s, want queued after the failed propose", tx.Content, info.Status)
		}
		if info.BlockHeight != 0 || info.BlockID != "" || info.ReplacedBy != "" {
			t.Fatalf("tx %q must not carry block/replace links: %+v", tx.Content, info)
		}
	}
	// 账户待处理列表、已确认序号与缺口不变，等待说明仍是 waiting-pack。
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{
		old.ID():  "waiting-pack",
		next.ID(): "waiting-pack",
	})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{b1.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{gapTx.ID(): "waiting-pack"})
	if a := n.Account(keys[0].pub); a.Gap != 0 {
		t.Fatalf("account0 gap = %d, want 0", a.Gap)
	}
	if a := n.Account(keys[2].pub); a.Gap != 1 {
		t.Fatalf("gap account gap = %d, want 1 (unchanged by the failed propose)", a.Gap)
	}
}

// TestProposeSaveFailureAtomic 覆盖首次生成本地提议保存失败的完整回归序列：
// 失败整体回滚（保存错误、结果为零值，轮次/高度/确认历史/交易状态/账户查询
// 全部照旧）-> 内存/磁盘/重开三处一致 -> 恢复后原可替换交易按正常规则替换、
// 重新生成的提议采用替换后的交易并遵守全部打包规则 -> 成功生成后冻结行为恢复。
func TestProposeSaveFailureAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 3)

	// 第 1 轮：确认账户0 的 seq1，留下一块确认历史，进入第 2 轮。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	if _, err := n.Submit(a1); err != nil {
		t.Fatal(err)
	}
	p1 := assertProposalOrder(t, n, []string{a1.ID()})
	confirmByVotes(t, n, keys, p1.BlockID)
	confirmedBlock, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("setup: round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}

	// 第 2 轮池内布局：账户0 的 seq2/seq3 接已确认序号 1 连续可打包，
	// 账户1 的 seq1 可打包，账户2 缺 seq1、seq2 费用再高也只能留在池中。
	old := NewTransaction(keys[0].priv, 2, []byte("old"), 20, 100)
	next := NewTransaction(keys[0].priv, 3, []byte("next"), 30, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
	gapTx := NewTransaction(keys[2].priv, 2, []byte("gap"), 99, 100)
	for _, tx := range []*Transaction{old, next, b1, gapTx} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}
	// 失败调用本应准备的列表：费用排序 b1(50)、old(20)，old 选入后 next(30)
	// 才具备条件，恰好取满单块上限 3 笔；gapTx 受序号缺口阻挡不得入选。
	failedList := []string{b1.ID(), old.ID(), next.ID()}
	failedBlockID := BlockID(2, 2, confirmedBlock.ID, failedList)
	assertProposeRolledBack(t, n, confirmedBlock, keys, old, next, b1, gapTx)

	// 快照首次提议前的磁盘文件：失败的保存不得改写它，也不得残留临时文件。
	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 首次 Propose 在此刻保存节点状态失败：必须返回保存错误，且不能同时
	// 给出可用的提议结果。
	n.injectSaveErr = errors.New("disk full (simulated)")
	view, err := n.Propose()
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("first propose must return the save error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if view.Round != 0 || view.BlockID != "" || len(view.TxIDs) != 0 || view.Empty {
		t.Fatalf("propose result must be the zero value on save failure, got %+v", view)
	}

	// 内存：轮次、高度、确认历史、交易状态与账户查询全部保持操作前结果，
	// 没有一份已经生效却没有保存的提议，也没有交易因此被锁住。
	assertProposeRolledBack(t, n, confirmedBlock, keys, old, next, b1, gapTx)

	// 磁盘：状态文件逐字节不变，目录中仅有 state.json。
	assertStateFileUnchanged(t, dir, before)

	// 重开节点：保存记录中没有本次提议造成的任何变化——仍没有本轮提议，
	// 原交易继续排队，确认历史保持原样。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertProposeRolledBack(t, reopened, confirmedBlock, keys, old, next, b1, gapTx)

	// 保存恢复正常后，对原本可替换的排队交易提交同发送者、同序号且费用
	// 严格更高的新交易：必须按一次正常替换成功处理，不能以 tx-in-proposal
	// 拒绝（失败的提议没有留下候选引用造成的限制）。
	newTx := NewTransaction(keys[0].priv, 2, []byte("new"), 60, 100)
	if newTx.ID() == old.ID() || newTx.Fee <= old.Fee {
		t.Fatal("setup: replacement must differ from old tx with a strictly higher fee")
	}
	res, err := reopened.Submit(newTx)
	if err != nil {
		t.Fatalf("replacement after recovery must succeed as a normal replacement: %v", err)
	}
	if res.TxID != newTx.ID() || res.ReplacedID != old.ID() || res.EvictedID != "" {
		t.Fatalf("replacement result = %+v, want {TxID:%s ReplacedID:%s EvictedID:\"\"}",
			res, newTx.ID(), old.ID())
	}
	oldInfo := reopened.mustTx(t, old.ID())
	if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != newTx.ID() {
		t.Fatalf("old tx = {status:%s replacedBy:%s}, want replaced by %s",
			oldInfo.Status, oldInfo.ReplacedBy, newTx.ID())
	}

	// 再生成本地提议：以此时池中的有效交易为准，采用替换后的交易标识，
	// 继续遵守账户序号连续、费用排序和单块上限——newTx(60)、b1(50)、next(30)，
	// 不复用失败调用中准备过的旧列表（含 old、顺序不同）。
	wantList := []string{newTx.ID(), b1.ID(), next.ID()}
	view, err = reopened.Propose()
	if err != nil {
		t.Fatalf("propose after recovery: %v", err)
	}
	if view.Round != 2 || view.Empty || fmt.Sprint(view.TxIDs) != fmt.Sprint(wantList) {
		t.Fatalf("regenerated proposal = %+v, want round 2 txs %v", view, wantList)
	}
	if wantID := BlockID(2, 2, confirmedBlock.ID, wantList); view.BlockID != wantID {
		t.Fatalf("regenerated block id = %s, want %s", view.BlockID, wantID)
	}
	if view.BlockID == failedBlockID {
		t.Fatal("regenerated proposal must not reuse the failed attempt's tx list")
	}
	// 提议生成后：选中的交易等待投票，缺口交易仍等待打包；已确认序号不推进。
	assertAccountQueue(t, reopened, keys[0].pub, 1, map[string]string{
		newTx.ID(): "waiting-vote",
		next.ID():  "waiting-vote",
	})
	assertAccountQueue(t, reopened, keys[1].pub, 0, map[string]string{b1.ID(): "waiting-vote"})
	assertAccountQueue(t, reopened, keys[2].pub, 0, map[string]string{gapTx.ID(): "waiting-pack"})
	if info := reopened.mustTx(t, gapTx.ID()); info.Status != StatusQueued {
		t.Fatalf("gap tx status = %s, want queued (sequence gap still blocks packing)", info.Status)
	}

	// 成功生成后才恢复既有的冻结行为：提议中的交易禁止替换。
	hiB := NewTransaction(keys[1].priv, 1, []byte("b1-hi"), 70, 100)
	if _, err := reopened.Submit(hiB); reason(err) != ReasonProposalLocked {
		t.Fatalf("replacing a proposed tx must be rejected with %s, got %v", ReasonProposalLocked, err)
	}
	// 后续新提交不改写这份提议：重复 Propose 与 Proposal 都返回同一提议。
	extra := NewTransaction(keys[3].priv, 1, []byte("extra"), 80, 100)
	if _, err := reopened.Submit(extra); err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if again.BlockID != view.BlockID || fmt.Sprint(again.TxIDs) != fmt.Sprint(wantList) {
		t.Fatalf("proposal must stay frozen after new submissions: %+v, want %+v", again, view)
	}
	p, ok := reopened.Proposal()
	if !ok || p.BlockID != view.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(wantList) {
		t.Fatalf("local proposal view changed after new submissions: %+v ok=%v", p, ok)
	}
	if info := reopened.mustTx(t, extra.ID()); info.Status != StatusQueued {
		t.Fatalf("late submission status = %s, want queued (not packed into the frozen proposal)", info.Status)
	}

	// 提议结果随再次落盘持久化：再次重开看到完全一致的状态。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	dp, ok := durable.Proposal()
	if !ok || dp.BlockID != view.BlockID || fmt.Sprint(dp.TxIDs) != fmt.Sprint(wantList) {
		t.Fatalf("durable proposal = %+v ok=%v, want %+v", dp, ok, view)
	}
	if info := durable.mustTx(t, old.ID()); info.Status != StatusReplaced || info.ReplacedBy != newTx.ID() {
		t.Fatalf("durable old tx wrong: %+v", info)
	}
	assertAccountQueue(t, durable, keys[0].pub, 1, map[string]string{
		newTx.ID(): "waiting-vote",
		next.ID():  "waiting-vote",
	})
	assertAccountQueue(t, durable, keys[2].pub, 0, map[string]string{gapTx.ID(): "waiting-pack"})
}

// assertEmptyProposeRolledBack 断言空块提议保存失败后节点完全处于操作前状态：
// 轮次 1、高度 0；本轮无本地提议、候选查询返回 unknown-round；因序号缺口
// 留在池中的交易仍 queued，账户缺口与 waiting-pack 说明不变。
func assertEmptyProposeRolledBack(t *testing.T, n *Node, key testKey, gapTx *Transaction) {
	t.Helper()
	if n.CurrentRound() != 1 {
		t.Fatalf("current round = %d, want 1 (failed empty propose must not advance the round)", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("latest confirmed block must not exist")
	}
	if p, ok := n.Proposal(); ok {
		t.Fatalf("round 1 must have no local proposal after the failed propose, got %+v", p)
	}
	if _, err := n.Candidates(1); reason(err) != ReasonUnknownRound {
		t.Fatalf("candidates for round 1 must be %s, got %v", ReasonUnknownRound, err)
	}
	info := n.mustTx(t, gapTx.ID())
	if info.Status != StatusQueued {
		t.Fatalf("gap tx status = %s, want queued after the failed empty propose", info.Status)
	}
	assertAccountQueue(t, n, key.pub, 0, map[string]string{gapTx.ID(): "waiting-pack"})
	if a := n.Account(key.pub); a.Gap != 1 {
		t.Fatalf("gap account gap = %d, want 1 (unchanged by the failed empty propose)", a.Gap)
	}
}

// TestProposeSaveFailureAtomicEmptyBlock 把“首次尝试生成空块”纳入同一项保障：
// 没有可打包交易时（池中仅有因序号缺口等待的交易），Propose 保存失败不登记
// 空块候选——内存/磁盘/重开三处都与操作前一致；保存恢复后才可生成正常的
// 空块提议，且生成后即冻结，后续提交不改写这份空块提议。
func TestProposeSaveFailureAtomicEmptyBlock(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 3)

	// 池中只有因序号缺口等待的交易：没有任何可打包交易，首次提议应为空块。
	gapTx := NewTransaction(keys[0].priv, 2, []byte("gap"), 99, 100)
	if _, err := n.Submit(gapTx); err != nil {
		t.Fatal(err)
	}
	assertEmptyProposeRolledBack(t, n, keys[0], gapTx)

	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 首次空块提议在此刻保存失败：返回保存错误，结果为零值。
	n.injectSaveErr = errors.New("disk full (simulated)")
	view, err := n.Propose()
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("first empty-block propose must return the save error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if view.Round != 0 || view.BlockID != "" || len(view.TxIDs) != 0 || view.Empty {
		t.Fatalf("propose result must be the zero value on save failure, got %+v", view)
	}

	// 内存：不登记空块候选，节点完全处于操作前状态。
	assertEmptyProposeRolledBack(t, n, keys[0], gapTx)

	// 磁盘：状态文件逐字节不变，目录中仅有 state.json。
	assertStateFileUnchanged(t, dir, before)

	// 重开节点：仍没有本轮提议，缺口交易继续排队。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertEmptyProposeRolledBack(t, reopened, keys[0], gapTx)

	// 保存恢复正常后：可生成正常的空块提议，标识按公开规则由轮次、高度、
	// 前块标识与空交易列表确定。
	view, err = reopened.Propose()
	if err != nil {
		t.Fatalf("empty-block propose after recovery: %v", err)
	}
	if view.Round != 1 || !view.Empty || len(view.TxIDs) != 0 {
		t.Fatalf("regenerated proposal = %+v, want an empty round-1 block", view)
	}
	if wantID := BlockID(1, 1, "", []string{}); view.BlockID != wantID {
		t.Fatalf("empty block id = %s, want %s", view.BlockID, wantID)
	}
	// 空块候选已登记：本地候选 pending、0 票，缺口交易仍等待打包。
	rc, err := reopened.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || !rc.HasRecords || len(rc.Candidates) != 1 {
		t.Fatalf("round 1 must have exactly one recorded pending candidate: %+v", rc)
	}
	c := rc.Candidates[0]
	if !c.Local || c.Result != CandidatePending || c.BlockID != view.BlockID || len(c.TxIDs) != 0 || len(c.Voters) != 0 {
		t.Fatalf("empty local candidate wrong: %+v", c)
	}
	assertAccountQueue(t, reopened, keys[0].pub, 0, map[string]string{gapTx.ID(): "waiting-pack"})

	// 空块提议生成后同样冻结：补上缺口序号使原交易变为可打包，提议也不改写。
	fill := NewTransaction(keys[0].priv, 1, []byte("fill"), 1, 100)
	if _, err := reopened.Submit(fill); err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if again.BlockID != view.BlockID || !again.Empty {
		t.Fatalf("empty proposal must stay frozen after new submissions: %+v, want %+v", again, view)
	}
	if info := reopened.mustTx(t, fill.ID()); info.Status != StatusQueued {
		t.Fatalf("late submission status = %s, want queued (not packed into the frozen empty proposal)", info.Status)
	}

	// 空块提议随再次落盘持久化：再次重开看到完全一致的状态。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	dp, ok := durable.Proposal()
	if !ok || dp.BlockID != view.BlockID || !dp.Empty {
		t.Fatalf("durable empty proposal = %+v ok=%v, want %+v", dp, ok, view)
	}
}
