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
// 场景：节点已有一块确认历史（第 1 轮确认，当前处于第 2 轮）。第 2 轮的本地
// 提议与一个竞争候选均已登记，两者引用的交易中有共享交易，也有仅被单个候选
// 引用的交易；两名验证者分别投给两个候选但都未达确认门槛（四验证者门槛为
// 3 票）。池内另有未被任何候选引用的排队交易（因序号缺口而未被本地提议打包）。
// 各交易到期轮次覆盖“恰好等于下一轮（2→3 后失效）”与“晚于下一轮（2→3 后
// 仍排队）”两种情况，使结束轮次确实同时涉及回到排队与过期两种结果。
//
// 当这次 EndRound 因节点状态写入失败而返回错误时：返回轮次值必须为 0；
// 候选落选标记、交易回到排队、轮次前进与到期失效作为一次完整操作必须整体不
// 生效——等待投票的交易不提前回排队，排队中的交易不提前过期，确认历史、
// 账户已确认序号、待处理交易与缺口说明全部保持操作前结果；磁盘状态文件逐字节
// 不变、不残留临时文件，重开节点看到同样的未结束状态。
//
// 保存恢复正常后，在仍处于原轮次的节点上再次 EndRound 才进入下一轮：
// 旧轮明确标记未确认结束、两候选均落选、原投票仍可查询；未到期的候选交易
// 回到排队（被两个候选共享的交易只保留一个池位）；到期轮次不大于新轮次的
// 交易从待处理列表移除并可查为 expired——共享的、竞争候选独有的、以及此前
// 没有被候选引用的交易都包括在内；此次结束不增加确认块、不推进账户已确认
// 序号。本组测试不改变现有公开入口、候选规则与确认门槛。

// maxTestTxs 固定为 3：第 2 轮池中有四笔可打包交易分属四个账户，费用最低的
// 一笔被排除在本地提议之外，恰好只能通过 RegisterCandidate 进入竞争候选。
const maxTestTxs = 3

// round2Tx 描述第 2 轮场景中的一笔交易及其候选归属。
type round2Tx struct {
	tx *Transaction
	// InLocal/InAlt 标记该交易是否被本地提议/竞争候选引用。
	InLocal bool
	InAlt   bool
}

// buildRoundTwoPending 在第 1 轮确认历史之上构造第 2 轮未决场景。
//
// 交易布局（账户0 第 1 轮已确认 seq1，其余账户确认序号为 0；账户4 不是
// 验证者，仅作发送者，其 seq2/seq3 因缺少 seq1 形成序号缺口、不会被打包）：
//
//	shared      账户0 seq2 fee10 expiry=3   本地+竞争 共享，恰好下一轮到期 -> 过期
//	localOnly   账户1 seq1 fee20 expiry=100 仅本地提议，晚到期           -> 回排队
//	altOnly     账户2 seq1 fee5  expiry=3   仅竞争候选，恰好下一轮到期   -> 过期
//	sharedLate  账户3 seq1 fee40 expiry=100 本地+竞争 共享，晚到期       -> 回排队
//	queuedExpire 账户4 seq2 fee1 expiry=3   候选外排队（带缺口）         -> 过期
//	queuedLate  账户4 seq3 fee1 expiry=100  候选外排队（带缺口）         -> 仍排队
//
// 单块上限为 3，本地提议按费用取 sharedLate、localOnly、shared 三笔；
// altOnly 费用最低、留在池内排队，只被竞争候选引用。
func buildRoundTwoPending(t *testing.T, n *Node, keys []testKey) (ProposalView, string, []round2Tx) {
	t.Helper()
	txs := []round2Tx{
		{tx: NewTransaction(keys[0].priv, 2, []byte("shared"), 10, 3), InLocal: true, InAlt: true},
		{tx: NewTransaction(keys[1].priv, 1, []byte("local-only"), 20, 100), InLocal: true},
		{tx: NewTransaction(keys[2].priv, 1, []byte("alt-only"), 5, 3), InAlt: true},
		{tx: NewTransaction(keys[3].priv, 1, []byte("shared-late"), 40, 100), InLocal: true, InAlt: true},
		{tx: NewTransaction(keys[4].priv, 2, []byte("queued-expire"), 1, 3)},
		{tx: NewTransaction(keys[4].priv, 3, []byte("queued-late"), 1, 100)},
	}
	for _, x := range txs {
		if _, err := n.Submit(x.tx); err != nil {
			t.Fatalf("submit %q: %v", x.tx.Content, err)
		}
	}

	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	// 打包顺序只取决于费用：sharedLate(40)、localOnly(20)、shared(10)，
	// 取满上限 3 笔；altOnly(5) 落选，queuedExpire/queuedLate 受序号缺口阻挡。
	wantLocal := []string{txs[3].tx.ID(), txs[1].tx.ID(), txs[0].tx.ID()}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint(wantLocal) {
		t.Fatalf("setup: local proposal order = %v, want %v", local.TxIDs, wantLocal)
	}

	// 竞争候选：竞争独有交易在前，两笔共享交易居中/收尾，顺序与本地提议不同。
	altList := []string{txs[2].tx.ID(), txs[0].tx.ID(), txs[3].tx.ID()}
	alt, err := n.RegisterCandidate(2, altList)
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID == local.BlockID {
		t.Fatal("setup: competitor block id must differ from local proposal")
	}

	// 验证者0 投本地、验证者1 投竞争候选，各 1 票；验证者2、3 未投票，均未决。
	if res, err := n.Vote(keys[0].pub, 2, local.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("local proposal vote: %+v %v", res, err)
	}
	if res, err := n.Vote(keys[1].pub, 2, alt.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("competitor vote: %+v %v", res, err)
	}
	return local, alt.BlockID, txs
}

// assertSameBlock 逐字段比较确认块（Block 含切片，不能直接用 == 比较）。
func assertSameBlock(t *testing.T, label string, got, want Block) {
	t.Helper()
	if got.Height != want.Height || got.Round != want.Round || got.ID != want.ID ||
		got.PreviousID != want.PreviousID || fmt.Sprint(got.TxIDs) != fmt.Sprint(want.TxIDs) {
		t.Fatalf("%s changed:\n got=%+v\nwant=%+v", label, got, want)
	}
}

// assertConfirmedHistory 断言第 1 轮确认历史未被后续操作改写：高度仍为 1、
// 最新块即给定块；第 1 轮候选记录保留、唯一候选胜出且三张确认票仍在。
func assertConfirmedHistory(t *testing.T, n *Node, confirmedBlock Block, keys []testKey) {
	t.Helper()
	if n.Height() != 1 {
		t.Fatalf("confirmed height = %d, want 1", n.Height())
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBlock(t, "block at height 1", blk, confirmedBlock)
	if latest, ok := n.LatestBlock(); !ok {
		t.Fatal("latest confirmed block disappeared")
	} else {
		assertSameBlock(t, "latest block", latest, confirmedBlock)
	}
	if _, err := n.BlockAt(2); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block at height 2: %v, want %s", err, ReasonUnknownBlock)
	}
	rc1, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc1.Ended || rc1.UnconfirmedEnd || !rc1.HasRecords || len(rc1.Candidates) != 1 {
		t.Fatalf("round 1 confirmed history changed: %+v", rc1)
	}
	won := rc1.Candidates[0]
	if won.Result != CandidateWon || won.BlockID != confirmedBlock.ID || len(won.Voters) != 3 {
		t.Fatalf("round 1 winner record changed: %+v", won)
	}
}

// assertRoundTwoPending 断言节点正处于第 2 轮结束前/失败后的完整未决状态：
// 轮次未前进；本地提议标识与交易顺序、两候选的 pending 结果、已有投票与未投票
// 名单原样；候选内四笔交易仍 waiting-vote（不提前回排队），候选外两笔仍
// waiting-pack（不提前过期）；账户确认序号、待处理集合与缺口说明保持操作前结果。
func assertRoundTwoPending(t *testing.T, n *Node, local ProposalView, altID string, txs []round2Tx, confirmedBlock Block, keys []testKey) {
	t.Helper()

	if n.CurrentRound() != 2 {
		t.Fatalf("current round = %d, want 2", n.CurrentRound())
	}
	assertConfirmedHistory(t, n, confirmedBlock, keys)

	rc, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 2 must stay pending with records: %+v", rc)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(rc.Candidates))
	}
	wantUnvoted := [][]byte{keys[2].pub, keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
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
		t.Fatalf("both candidates must remain present: %+v", rc.Candidates)
	}
	wantLocalOrder := []string{txs[3].tx.ID(), txs[1].tx.ID(), txs[0].tx.ID()}
	if !gotLocal.Local || gotLocal.Result != CandidatePending ||
		fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint(wantLocalOrder) ||
		len(gotLocal.Voters) != 1 || fmt.Sprintf("%x", gotLocal.Voters[0]) != fmt.Sprintf("%x", keys[0].pub) {
		t.Fatalf("local candidate must stay pending with its order and single vote: %+v", gotLocal)
	}
	wantAltOrder := []string{txs[2].tx.ID(), txs[0].tx.ID(), txs[3].tx.ID()}
	if gotAlt.Local || gotAlt.Result != CandidatePending ||
		fmt.Sprint(gotAlt.TxIDs) != fmt.Sprint(wantAltOrder) ||
		len(gotAlt.Voters) != 1 || fmt.Sprintf("%x", gotAlt.Voters[0]) != fmt.Sprintf("%x", keys[1].pub) {
		t.Fatalf("competitor must stay pending with its order and single vote: %+v", gotAlt)
	}

	// 本地提议视图始终返回同一份标识与交易顺序。
	p, ok := n.Proposal()
	if !ok {
		t.Fatal("local proposal must still be available after the failed end")
	}
	if p.Round != 2 || p.BlockID != local.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(wantLocalOrder) {
		t.Fatalf("local proposal changed: %+v, want id=%s order=%v", p, local.BlockID, wantLocalOrder)
	}

	// 候选内交易保持 proposed，候选外保持 queued；都不得关联确认块。
	for _, x := range txs {
		info := n.mustTx(t, x.tx.ID())
		want := StatusQueued
		if x.InLocal || x.InAlt {
			want = StatusProposed
		}
		if info.Status != want {
			t.Fatalf("tx %q status = %s, want %s", x.tx.Content, info.Status, want)
		}
		if info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("tx %q must not reference a block: height=%d block=%q",
				x.tx.Content, info.BlockHeight, info.BlockID)
		}
	}

	// 账户查询：确认序号、待处理交易与缺口说明全部保持操作前结果。
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{txs[0].tx.ID(): "waiting-vote"})
	if a := n.Account(keys[0].pub); a.Gap != 0 {
		t.Fatalf("account 0 gap = %d, want 0", a.Gap)
	}
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{txs[1].tx.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{txs[2].tx.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[3].pub, 0, map[string]string{txs[3].tx.ID(): "waiting-vote"})
	// 账户4 两笔候选外排队交易均 waiting-pack；缺少 seq1，最早缺口为 1。
	assertAccountQueue(t, n, keys[4].pub, 0, map[string]string{
		txs[4].tx.ID(): "waiting-pack",
		txs[5].tx.ID(): "waiting-pack",
	})
	if a := n.Account(keys[4].pub); a.Gap != 1 {
		t.Fatalf("account 4 gap = %d, want 1", a.Gap)
	}
}

// assertRoundThreeEnded 断言保存恢复后再次 EndRound 成功进入第 3 轮后的状态。
func assertRoundThreeEnded(t *testing.T, n *Node, local ProposalView, altID string, txs []round2Tx, confirmedBlock Block, keys []testKey) {
	t.Helper()
	const newRound = uint64(3)

	if n.CurrentRound() != newRound {
		t.Fatalf("current round = %d, want 3", n.CurrentRound())
	}
	// 主动结束不留确认块：高度与确认历史完全不变，也不存在高度 2 的块。
	assertConfirmedHistory(t, n, confirmedBlock, keys)

	// 旧轮明确标记未确认结束，两候选均落选，原投票与未投票名单仍可查询。
	rc, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || !rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 2 must be an unconfirmed ended round with records: %+v", rc)
	}
	wantUnvoted := [][]byte{keys[2].pub, keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(rc.Candidates))
	}
	for _, c := range rc.Candidates {
		if c.Result != CandidateLost {
			t.Fatalf("candidate %s result = %s, want lost", c.BlockID, c.Result)
		}
		var wantVoter []byte
		switch c.BlockID {
		case local.BlockID:
			if !c.Local {
				t.Fatalf("local candidate flag must be retained: %+v", c)
			}
			wantVoter = keys[0].pub
		case altID:
			wantVoter = keys[1].pub
		default:
			t.Fatalf("unexpected candidate %s", c.BlockID)
		}
		if len(c.Voters) != 1 || fmt.Sprintf("%x", c.Voters[0]) != fmt.Sprintf("%x", wantVoter) {
			t.Fatalf("candidate %s original vote not retained: %x", c.BlockID, c.Voters)
		}
	}

	// 新轮次尚未提议：Proposal 不再返回第 2 轮的冻结提议。
	if p, ok := n.Proposal(); ok {
		t.Fatalf("round 2 proposal must not be exposed in round 3: %+v", p)
	}

	// 逐笔交易：到期轮次不大于新轮次的查为 expired（含共享、竞争独有、候选外），
	// 未到期的候选交易回到排队，候选外晚到期交易保持排队；都不带确认块关联。
	for _, x := range txs {
		info := n.mustTx(t, x.tx.ID())
		want := StatusQueued
		if x.tx.Expiry <= newRound {
			want = StatusExpired
		}
		if info.Status != want {
			t.Fatalf("tx %q status = %s, want %s (expiry %d)",
				x.tx.Content, info.Status, want, x.tx.Expiry)
		}
		if info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("tx %q must not reference a confirmed block: height=%d block=%q",
				x.tx.Content, info.BlockHeight, info.BlockID)
		}
	}

	// 账户0：shared(seq2) 过期移出，已确认序号仍为 1，待处理为空。
	if a := n.Account(keys[0].pub); a.ConfirmedSequence != 1 || len(a.Pending) != 0 {
		t.Fatalf("account 0 after end = %+v, want confirmed seq 1 with no pending", a)
	}
	// 账户1：本地独有交易晚到期，回到排队等待打包；序号不推进、无缺口。
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{txs[1].tx.ID(): "waiting-pack"})
	// 账户2：竞争独有交易恰好到期，移出待处理，查为过期；序号不推进。
	if a := n.Account(keys[2].pub); a.ConfirmedSequence != 0 || len(a.Pending) != 0 {
		t.Fatalf("account 2 after end = %+v, want no pending tx", a)
	}
	// 账户3：共享的晚到期交易回到排队，两个候选只留下一个池位、待处理恰一笔。
	assertAccountQueue(t, n, keys[3].pub, 0, map[string]string{txs[3].tx.ID(): "waiting-pack"})
	// 账户4：候选外早到期交易移出，晚到期交易仍排队；序号缺口（缺 seq1）照常指出。
	assertAccountQueue(t, n, keys[4].pub, 0, map[string]string{txs[5].tx.ID(): "waiting-pack"})
	if a := n.Account(keys[4].pub); a.Gap != 1 {
		t.Fatalf("account 4 gap after end = %d, want 1", a.Gap)
	}
}

// TestEndRoundSaveFailureAtomic 覆盖主动结束未确认轮次保存失败的完整回归序列：
// 失败整体回滚（返回轮次为 0，内存/磁盘/重开三处一致）-> 恢复后在原轮次再次
// 结束才进入下一轮，落选标记、交易回排队（共享一份）、过期与不留块全部生效并
// 再次持久化。
func TestEndRoundSaveFailureAtomic(t *testing.T) {
	// 前五把密钥中前四把为验证者，第五把仅作交易发送者。
	keys := genKeys(t, 5)
	dir := t.TempDir()
	vals := make([][]byte, 4)
	for i := range vals {
		vals[i] = keys[i].pub
	}
	n, err := New(dir, Config{Seed: []byte("test-seed-v1"), Validators: vals, MaxTxsPerBlock: maxTestTxs})
	if err != nil {
		t.Fatal(err)
	}

	// 第 1 轮确认历史：账户0 seq1 上链（高度 1），确认后节点进入第 2 轮。
	hist := NewTransaction(keys[0].priv, 1, []byte("round1-confirmed"), 5, 100)
	if _, err := n.Submit(hist); err != nil {
		t.Fatal(err)
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p1.TxIDs) != fmt.Sprint([]string{hist.ID()}) {
		t.Fatalf("setup: round 1 proposal = %v, want [%s]", p1.TxIDs, hist.ID())
	}
	// 四验证者门槛为 3 票；不能用 confirmCandidate——它按 len(keys) 计票，
	// 而本测试的 keys[4] 不是验证者。手动投出三张确认票。
	for i := 0; i < 3; i++ {
		res, err := n.Vote(keys[i].pub, 1, p1.BlockID)
		if err != nil || !res.Counted {
			t.Fatalf("round 1 confirming vote %d: %+v %v", i+1, res, err)
		}
		if (i == 2) != res.Confirmed {
			t.Fatalf("round 1 vote %d confirmed=%v, want %v", i+1, res.Confirmed, i == 2)
		}
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("setup: round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	confirmedBlock, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}

	local, altID, txs := buildRoundTwoPending(t, n, keys)
	assertRoundTwoPending(t, n, local, altID, txs, confirmedBlock, keys)

	// 快照结束前的磁盘文件：失败的保存既不得改写它，也不得残留临时文件。
	statePath := filepath.Join(dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 主动结束因节点状态写入失败而返回错误：返回轮次必须为 0。
	n.injectSaveErr = errors.New("disk full (simulated)")
	nextRound, err := n.EndRound()
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("EndRound must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if nextRound != 0 {
		t.Fatalf("EndRound return round = %d, want 0 on save failure", nextRound)
	}

	// 内存：轮次、候选、票、未投票名单、提议、每笔交易状态与账户查询全部照旧。
	assertRoundTwoPending(t, n, local, altID, txs, confirmedBlock, keys)

	// 磁盘：状态文件逐字节保持操作前内容，目录中仅有 state.json。
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("state file changed despite the failed save")
	}
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range dirEntries {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[state.json]" {
		t.Fatalf("failed save must not leave temp files, dir contains %v", names)
	}

	// 重开节点：保存记录中没有本次结束造成的任何变化，看到同样的未结束状态。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRoundTwoPending(t, reopened, local, altID, txs, confirmedBlock, keys)

	// 保存恢复正常后，节点仍处于原轮次；再次 EndRound 才进入第 3 轮。
	nextRound, err = reopened.EndRound()
	if err != nil {
		t.Fatalf("EndRound after recovery failed: %v", err)
	}
	if nextRound != 3 {
		t.Fatalf("EndRound after recovery = %d, want 3", nextRound)
	}
	assertRoundThreeEnded(t, reopened, local, altID, txs, confirmedBlock, keys)

	// 成功的结束同样持久化：再开一次看到完全一致的第 3 轮状态。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRoundThreeEnded(t, durable, local, altID, txs, confirmedBlock, keys)
}
