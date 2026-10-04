package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住“首次生成本地提议时保存失败”的原子落盘回归保障。
//
// 场景：节点已有确认历史（第 1 轮确认了一个块，进入第 2 轮），交易池中同时有
// 能够连续打包的交易（a2、b1）和因账户序号缺口而留在池中的交易（c2）。本轮尚
// 无本地提议时首次调用 Propose，本应选出交易、建立候选并把选中交易置为等待
// 投票，但这一次保存节点状态失败。该调用必须整体不生效：返回错误且结果为空，
// 用户看到的节点与调用前完全一致——没有已生效却未保存的提议，本轮仍无候选
// 记录，没有交易被置为等待投票，轮次、确认高度与确认历史不变，磁盘文件逐字节
// 保持原样。失败也不能留下候选引用造成的限制：保存恢复后，原本可替换的排队
// 交易仍可按同发送者、同序号、费用严格更高的规则替换，不能以 tx-in-proposal
// 拒绝；再生成提议必须以此时池中的有效交易为准（采用替换后的标识、遵守序号
// 连续、费用排序与单块上限），不得复用失败调用中准备过的旧列表。成功生成后
// 才恢复既有冻结行为。空块首次生成遵守同一规则。
// 本组测试不新增入口，也不改变既有拒绝原因与正常提议结果。

// assertRound2PrePropose 断言节点正处于第 2 轮首次提议前/失败后的状态：
// 轮次 2、确认高度 1；第 1 轮确认块标识与交易顺序不变；本轮无本地提议、
// Candidates(2) 按既有规则返回 unknown-round；a2、b1、c2 全部 queued，
// 各账户已确认序号、待处理列表与最早缺口不变，等待说明均为 waiting-pack。
func assertRound2PrePropose(t *testing.T, n *Node, block1 Block, a2, b1, c2 *Transaction, keys []testKey) {
	t.Helper()
	if n.CurrentRound() != 2 {
		t.Fatalf("current round = %d, want 2", n.CurrentRound())
	}
	if n.Height() != 1 {
		t.Fatalf("confirmed height = %d, want 1", n.Height())
	}
	// 原有确认历史：标识、高度、前块与交易顺序都不能被失败的提议改写。
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != block1.ID || blk.Round != 1 || blk.Height != 1 || blk.PreviousID != "" ||
		fmt.Sprint(blk.TxIDs) != fmt.Sprint(block1.TxIDs) {
		t.Fatalf("confirmed block at height 1 changed: %+v, want %+v", blk, block1)
	}
	if latest, ok := n.LatestBlock(); !ok || latest.ID != block1.ID || fmt.Sprint(latest.TxIDs) != fmt.Sprint(block1.TxIDs) {
		t.Fatalf("latest block = %+v ok=%v, want unchanged block %s", latest, ok, block1.ID)
	}

	// 本轮仍无本地提议：没有一份已生效却未保存的提议。
	if p, ok := n.Proposal(); ok {
		t.Fatalf("round 2 must have no local proposal yet, got %+v", p)
	}
	// 本轮尚无候选记录：按既有规则返回 unknown-round。
	if _, err := n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("Candidates(2) reason = %q (err=%v), want %q", reason(err), err, ReasonUnknownRound)
	}
	// 本地提议未产生时不能登记候选，拒绝原因不变。
	if _, err := n.RegisterCandidate(2, []string{a2.ID()}); reason(err) != ReasonNoProposal {
		t.Fatalf("RegisterCandidate before proposal reason = %q (err=%v), want %q", reason(err), err, ReasonNoProposal)
	}
	// 第 1 轮历史记录保持已确认结束。
	rc1, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc1.Ended || rc1.UnconfirmedEnd || !rc1.HasRecords {
		t.Fatalf("round 1 must stay a confirmed, recorded round: %+v", rc1)
	}

	// 没有交易因失败的提议被置为等待投票：全部仍是 queued。
	for _, tx := range []*Transaction{a2, b1, c2} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusQueued {
			t.Fatalf("tx %s status = %s, want queued (no tx may be locked by a failed propose)", tx.ID(), info.Status)
		}
		if info.BlockHeight != 0 || info.BlockID != "" || info.ReplacedBy != "" {
			t.Fatalf("tx %s must not reference a block or replacement: %+v", tx.ID(), info)
		}
	}
	// 账户待处理列表、已确认序号与最早缺口不变，说明仍是 waiting-pack。
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{a2.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{b1.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{c2.ID(): "waiting-pack"})
	if a := n.Account(keys[2].pub); a.Gap != 1 {
		t.Fatalf("c2 account gap = %d, want 1 (sequence gap unchanged)", a.Gap)
	}
}

// TestProposeSaveFailureAtomic 覆盖首次提议保存失败的完整回归序列：
// 失败整体回滚 -> 内存/磁盘/重开三处一致 -> 恢复后旧交易可正常替换 ->
// 再提议以当前池为准生成新列表 -> 成功后恢复冻结行为并持久化。
func TestProposeSaveFailureAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 3)

	// 第 1 轮：确认一个块，建立确认历史。x1 确认后账户 0 的已确认序号为 1。
	x1 := NewTransaction(keys[0].priv, 1, []byte("x1"), 1, 100)
	if _, err := n.Submit(x1); err != nil {
		t.Fatal(err)
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p1.TxIDs) != fmt.Sprint([]string{x1.ID()}) {
		t.Fatalf("setup: round 1 proposal = %v, want [x1]", p1.TxIDs)
	}
	var block1 Block
	for i, k := range keys[:3] {
		res, err := n.Vote(k.pub, 1, p1.BlockID)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && res.Confirmed {
			t.Fatalf("setup: vote %d must not confirm before quorum", i+1)
		}
		if i == 2 {
			if !res.Confirmed || res.Block == nil {
				t.Fatalf("setup: third vote must confirm: %+v", res)
			}
			block1 = *res.Block
		}
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("setup: round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}

	// 第 2 轮池：a2（账户 0 序号 2，费 10）与 b1（账户 1 序号 1，费 5）可连续打包；
	// c2（账户 2 序号 2，费 100）因序号 1 缺口留在池中，费用再高也不能入选。
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	c2 := NewTransaction(keys[2].priv, 2, []byte("c2"), 100, 100)
	for _, tx := range []*Transaction{a2, b1, c2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	assertRound2PrePropose(t, n, block1, a2, b1, c2, keys)

	// 记录首次提议前的磁盘文件：失败的保存不得改动它。
	statePath := filepath.Join(dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 首次生成本地提议，但此刻保存节点状态失败。
	n.injectSaveErr = errors.New("disk full (simulated)")
	view, err := n.Propose()
	if err == nil {
		t.Fatal("first propose must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if view.Round != 0 || view.BlockID != "" || view.TxIDs != nil || view.Empty {
		t.Fatalf("propose must not yield a usable view on save failure, got %+v", view)
	}
	// 错误仍在时再次调用也必须失败：失败调用没有留下可重复返回的提议。
	if _, err := n.Propose(); err == nil {
		t.Fatal("propose must keep failing while saves fail; no proposal may take effect without being saved")
	}
	n.injectSaveErr = nil

	// 内存：轮次、高度、确认历史、交易与账户全部照旧，本轮仍无提议与候选。
	assertRound2PrePropose(t, n, block1, a2, b1, c2, keys)

	// 磁盘：状态文件逐字节保持提议前内容，且不残留临时文件。
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

	// 重开节点：尚未成功重试前，磁盘上同样没有本轮提议，原交易继续排队，
	// 确认历史保持原样。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRound2PrePropose(t, reopened, block1, a2, b1, c2, keys)

	// 保存恢复后，对原本可替换的排队交易提交同发送者、同序号且费用严格更高
	// 的新交易：必须按原有规则成功替换，失败的提议不能留下 tx-in-proposal 限制。
	b1v2 := NewTransaction(keys[1].priv, 1, []byte("b1-v2"), 50, 100)
	sub, err := reopened.Submit(b1v2)
	if err != nil {
		t.Fatalf("higher-fee replacement after failed propose must succeed, got %v", err)
	}
	if sub.TxID != b1v2.ID() || sub.ReplacedID != b1.ID() || sub.EvictedID != "" {
		t.Fatalf("replacement result = %+v, want tx=%s replaced=%s", sub, b1v2.ID(), b1.ID())
	}
	oldInfo := reopened.mustTx(t, b1.ID())
	if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != b1v2.ID() {
		t.Fatalf("replaced tx wrong: %+v", oldInfo)
	}

	// 再生成提议：以此时池中的有效交易为准——采用替换后的 b1v2，且费用 50
	// 高于 a2 的 10，顺序为 [b1v2, a2]；缺口交易 c2 仍不能入选。若复用失败
	// 调用准备的旧列表，这里会得到 [a2, b1]。
	want := []string{b1v2.ID(), a2.ID()}
	p, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("proposal after recovery = %v, want %v (rebuilt from the current pool, not the failed attempt's list)", p.TxIDs, want)
	}
	if p.Round != 2 || p.Empty {
		t.Fatalf("proposal view wrong: %+v", p)
	}
	if wantID := BlockID(2, 2, block1.ID, want); p.BlockID != wantID {
		t.Fatalf("proposal block id = %s, want %s (round 2, height 2, prev %s)", p.BlockID, wantID, block1.ID)
	}

	// 成功生成后：选中交易进入等待投票，缺口交易仍等待打包。
	assertAccountQueue(t, reopened, keys[0].pub, 1, map[string]string{a2.ID(): "waiting-vote"})
	assertAccountQueue(t, reopened, keys[1].pub, 0, map[string]string{b1v2.ID(): "waiting-vote"})
	assertAccountQueue(t, reopened, keys[2].pub, 0, map[string]string{c2.ID(): "waiting-pack"})
	if info := reopened.mustTx(t, c2.ID()); info.Status != StatusQueued {
		t.Fatalf("gap tx status = %s, want queued", info.Status)
	}

	// 成功生成后才恢复冻结：新提交的交易不改写已产生的提议。
	d1 := NewTransaction(keys[3].priv, 1, []byte("d1"), 1000, 100)
	if _, err := reopened.Submit(d1); err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if again.BlockID != p.BlockID || fmt.Sprint(again.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("proposal must stay frozen after success, got id=%s txs=%v", again.BlockID, again.TxIDs)
	}
	assertAccountQueue(t, reopened, keys[3].pub, 0, map[string]string{d1.ID(): "waiting-pack"})
	// 已提议交易恢复禁止替换。
	a2v2 := NewTransaction(keys[0].priv, 2, []byte("a2-v2"), 99, 100)
	if _, err := reopened.Submit(a2v2); reason(err) != ReasonProposalLocked {
		t.Fatalf("replacing a proposed tx reason = %q (err=%v), want %q", reason(err), err, ReasonProposalLocked)
	}

	// 提议结果随落盘持久化：再次重开后看到的仍是同一份提议。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	dp, ok := durable.Proposal()
	if !ok {
		t.Fatal("reopened node must have the round 2 local proposal")
	}
	if dp.BlockID != p.BlockID || fmt.Sprint(dp.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("durable proposal = id=%s txs=%v, want id=%s txs=%v", dp.BlockID, dp.TxIDs, p.BlockID, want)
	}
	rc, err := durable.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || len(rc.Candidates) != 1 || !rc.Candidates[0].Local ||
		rc.Candidates[0].Result != CandidatePending ||
		fmt.Sprint(rc.Candidates[0].TxIDs) != fmt.Sprint(want) {
		t.Fatalf("round 2 candidates wrong after reopen: %+v", rc)
	}
	if info := durable.mustTx(t, a2.ID()); info.Status != StatusProposed {
		t.Fatalf("durable a2 status = %s, want proposed", info.Status)
	}
	if info := durable.mustTx(t, c2.ID()); info.Status != StatusQueued {
		t.Fatalf("durable c2 status = %s, want queued", info.Status)
	}
	if blk, err := durable.BlockAt(1); err != nil || blk.ID != block1.ID {
		t.Fatalf("durable confirmed history changed: %+v %v", blk, err)
	}
}

// TestProposeSaveFailureEmptyBlockAtomic 覆盖空块首次生成时保存失败的回归：
// 池中没有可打包交易（仅有序号缺口的交易）时，失败的空块提议同样整体不生效、
// 不登记空块候选；保存恢复后才能生成正常的空块提议，且空提议不锁定任何交易。
func TestProposeSaveFailureEmptyBlockAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 5)

	// 只有一笔因序号 1 缺口而无法打包的交易：本轮只能产生空块。
	g2 := NewTransaction(keys[0].priv, 2, []byte("g2"), 7, 100)
	if _, err := n.Submit(g2); err != nil {
		t.Fatal(err)
	}

	assertNoProposalEmptyPool := func(t *testing.T, n *Node) {
		t.Helper()
		if n.CurrentRound() != 1 || n.Height() != 0 {
			t.Fatalf("round=%d height=%d, want 1/0", n.CurrentRound(), n.Height())
		}
		if _, ok := n.LatestBlock(); ok {
			t.Fatal("no confirmed block may exist")
		}
		if p, ok := n.Proposal(); ok {
			t.Fatalf("round 1 must have no local proposal yet, got %+v", p)
		}
		if _, err := n.Candidates(1); reason(err) != ReasonUnknownRound {
			t.Fatalf("Candidates(1) reason = %q (err=%v), want %q", reason(err), err, ReasonUnknownRound)
		}
		if info := n.mustTx(t, g2.ID()); info.Status != StatusQueued {
			t.Fatalf("gap tx status = %s, want queued", info.Status)
		}
		assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{g2.ID(): "waiting-pack"})
		if a := n.Account(keys[0].pub); a.Gap != 1 {
			t.Fatalf("gap = %d, want 1", a.Gap)
		}
	}
	assertNoProposalEmptyPool(t, n)

	statePath := filepath.Join(dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 首次尝试生成空块，保存失败：不登记空块候选，状态整体不变。
	n.injectSaveErr = errors.New("disk full (simulated)")
	view, err := n.Propose()
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("empty-block propose must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if view.Round != 0 || view.BlockID != "" || view.TxIDs != nil || view.Empty {
		t.Fatalf("propose must not yield a usable view on save failure, got %+v", view)
	}
	assertNoProposalEmptyPool(t, n)

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("state file changed despite the failed save")
	}

	// 重开节点：磁盘上同样没有本轮提议，缺口交易继续排队。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertNoProposalEmptyPool(t, reopened)

	// 保存恢复后：可正常生成空块提议，标识按公开规则由空交易列表确定。
	p, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty || len(p.TxIDs) != 0 || p.Round != 1 {
		t.Fatalf("empty proposal wrong: %+v", p)
	}
	if wantID := BlockID(1, 1, "", []string{}); p.BlockID != wantID {
		t.Fatalf("empty block id = %s, want %s", p.BlockID, wantID)
	}
	// 重复请求返回同一空块提议。
	again, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if again.BlockID != p.BlockID {
		t.Fatalf("repeated propose = %s, want frozen %s", again.BlockID, p.BlockID)
	}
	// 空提议不引用任何交易：缺口交易仍排队等待打包，不被锁定。
	assertAccountQueue(t, reopened, keys[0].pub, 0, map[string]string{g2.ID(): "waiting-pack"})
	rc, err := reopened.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || len(rc.Candidates) != 1 || !rc.Candidates[0].Local ||
		rc.Candidates[0].Result != CandidatePending || len(rc.Candidates[0].TxIDs) != 0 {
		t.Fatalf("round 1 candidates wrong: %+v", rc)
	}

	// 空块提议生成后同样冻结：此时补齐缺口使交易可打包，提议也不改写。
	g1 := NewTransaction(keys[0].priv, 1, []byte("g1"), 9, 100)
	if _, err := reopened.Submit(g1); err != nil {
		t.Fatal(err)
	}
	frozen, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if frozen.BlockID != p.BlockID || !frozen.Empty {
		t.Fatalf("empty proposal must stay frozen, got %+v", frozen)
	}
	assertAccountQueue(t, reopened, keys[0].pub, 0, map[string]string{
		g1.ID(): "waiting-pack", g2.ID(): "waiting-pack",
	})

	// 空块提议随落盘持久化。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	dp, ok := durable.Proposal()
	if !ok || !dp.Empty || dp.BlockID != p.BlockID {
		t.Fatalf("durable empty proposal = %+v ok=%v, want id=%s", dp, ok, p.BlockID)
	}
}
