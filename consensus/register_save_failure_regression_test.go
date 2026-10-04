package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住“非空竞争候选登记”的原子落盘回归保障，重点是登记保存失败时
// 不能提前锁住原本仍可加费替换的排队交易。
//
// 场景：第 1 轮本地提议已产生且尚未确认（四验证者确认门槛为 3 票）。本地提议
// 按费用打包 a1、b1 两笔（单块上限 2）；另一个已登记的竞争候选只引用 a1，
// 并持有两张未达门槛的票，本地候选 0 票，两名验证者未投票。池中还有一笔
// 没有被任何候选引用的排队交易 c1。准备登记的新竞争候选为非空列表 [c1, a1]：
// 既包含此前只在池中排队的 c1，也包含已被现有候选引用的 a1；列表不超过单块
// 上限、各账户序号自 1 起连续、交易均未到期，按现有规则本应登记成功。
//
// 登记成功的公开效果是：新候选入册，其首次引用的排队交易 c1 改为等待投票，
// 此后同发送者同序号的加费替换会以 tx-in-proposal 被拒。因此当这次登记因
// 节点状态保存失败而结束时，必须整体不生效：调用收到保存错误而非登记结果；
// 按轮次查询仍只有原来的两个候选，本地提议的区块标识与交易顺序、各候选的
// 投票者与未投票名单都保持登记前结果，轮次与确认高度不前进；c1 仍显示排队、
// 账户仍说明等待打包，同发送者同序号、费用严格更高的新交易仍可替换它；
// a1、b1 等此前已被候选引用的交易继续等待投票，加费替换仍以现有的锁定原因
// 拒绝，不能因为新候选登记失败而被解除保护；两类交易的完整内容都不被改写，
// 账户已确认序号不推进。磁盘状态文件逐字节不变、不残留临时文件，重新打开
// 状态目录看到与失败后原节点相同的候选、投票与交易状态。
//
// 保存恢复正常后分两条路径钉住现有公开行为：
//   - 已对 c1 做过加费替换时，旧交易按原规则显示 replaced 并指向新交易，
//     原候选列表因引用已退出池的交易不能原样登记；
//   - 尚未替换列表中的交易时，重新提交原来的合法候选列表应成功登记并被
//     识别为新候选（0 票），届时 c1 才改为等待投票，原有票数仍归属于原来
//     的候选，本地提议保持冻结、不直接确认区块。
//
// 本组测试不新增公开入口，也不改变现有登记、投票与交易替换规则。

// registerScene 收拢“竞争候选登记保存失败”场景的关键句柄。
type registerScene struct {
	local  ProposalView
	alt1ID string
	// a1 被本地提议与既有竞争候选 alt1 共享引用：等待投票、禁止替换。
	a1 *Transaction
	// b1 仅被本地提议引用：等待投票、禁止替换。
	b1 *Transaction
	// c1 没有被任何候选引用，仅在池中排队：等待打包、可加费替换；
	// 它是失败的新候选首次引用的排队交易。
	c1 *Transaction
	// failedList 是保存失败的那次登记本应登记的非空交易列表。
	failedList []string
}

// buildRegisterScene 在一个单块上限为 2 的新节点上构造登记前的未决场景：
// 本地提议 [a1, b1]（费用 10、5），竞争候选 alt1=[a1] 持两票，c1 仅排队。
func buildRegisterScene(t *testing.T, n *Node, keys []testKey) registerScene {
	t.Helper()
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), 1, 100)
	for _, tx := range []*Transaction{a1, b1, c1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}
	// 单块上限 2：本地提议按费用取 a1(10)、b1(5)；c1(1) 留在池中排队。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("setup: local proposal = %v, want [a1 b1]", local.TxIDs)
	}

	// 既有竞争候选只引用共享交易 a1，与本地提议标识不同。
	alt1, err := n.RegisterCandidate(1, []string{a1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt1.Existing || alt1.BlockID == local.BlockID {
		t.Fatalf("setup: existing competitor must be a fresh candidate: %+v local=%s", alt1, local.BlockID)
	}

	// 验证者1、2 投给 alt1，各一票：两票未达四验证者的 3 票门槛；
	// 验证者0、3 未投票，本地候选 0 票。
	for _, i := range []int{1, 2} {
		res, err := n.Vote(keys[i].pub, 1, alt1.BlockID)
		if err != nil || !res.Counted || res.Confirmed {
			t.Fatalf("setup vote for alt1 by validator %d: %+v %v", i, res, err)
		}
	}

	failedList := []string{c1.ID(), a1.ID()} // 顺序与本地提议不同，区块标识必然不同
	return registerScene{
		local:      local,
		alt1ID:     alt1.BlockID,
		a1:         a1,
		b1:         b1,
		c1:         c1,
		failedList: failedList,
	}
}

// sceneCandidates 断言登记前/失败后始终不变的那部分轮次状态：
// 轮次 1、确认高度 0 且无确认块；按轮次查询恰有两个 pending 候选——
// 本地提议标识与交易顺序不变、0 票；alt1 保留两张原票；未投票名单为
// 验证者0、3。同时断言 a1、b1 继续等待投票且内容完整。
func sceneCandidates(t *testing.T, n *Node, sc registerScene, keys []testKey) {
	t.Helper()
	if n.CurrentRound() != 1 {
		t.Fatalf("current round = %d, want 1 (failed register must not advance the round)", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("no confirmed block may exist after the failed register")
	}
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block at height 1: %v, want %s", err, ReasonUnknownBlock)
	}

	// 本地提议始终返回同一份冻结结果。
	p, ok := n.Proposal()
	if !ok {
		t.Fatal("local proposal must stay available after the failed register")
	}
	if p.Round != 1 || p.BlockID != sc.local.BlockID ||
		fmt.Sprint(p.TxIDs) != fmt.Sprint(sc.local.TxIDs) {
		t.Fatalf("local proposal changed: %+v, want id=%s order=%v", p, sc.local.BlockID, sc.local.TxIDs)
	}

	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 1 must stay pending with records: %+v", rc)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2 (the failed candidate must not be registered)", len(rc.Candidates))
	}
	wantUnvoted := [][]byte{keys[0].pub, keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
	}

	var gotLocal, gotAlt *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case sc.local.BlockID:
			gotLocal = &rc.Candidates[i]
		case sc.alt1ID:
			gotAlt = &rc.Candidates[i]
		}
	}
	if gotLocal == nil || gotAlt == nil {
		t.Fatalf("only the original candidates must remain: %+v", rc.Candidates)
	}
	if !gotLocal.Local || gotLocal.Result != CandidatePending ||
		fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint([]string{sc.a1.ID(), sc.b1.ID()}) ||
		len(gotLocal.Voters) != 0 {
		t.Fatalf("local candidate must stay pending with its order and no votes: %+v", gotLocal)
	}
	if gotAlt.Local || gotAlt.Result != CandidatePending ||
		fmt.Sprint(gotAlt.TxIDs) != fmt.Sprint([]string{sc.a1.ID()}) {
		t.Fatalf("existing competitor must stay pending with its own tx order: %+v", gotAlt)
	}
	wantAltVoters := [][]byte{keys[1].pub, keys[2].pub}
	if fmt.Sprint(hexKeys(gotAlt.Voters)) != fmt.Sprint(hexKeys(wantAltVoters)) {
		t.Fatalf("existing competitor votes = %x, want %x", gotAlt.Voters, wantAltVoters)
	}

	// 失败候选的区块标识绝不能出现在候选集合中。
	failedBlockID := BlockID(1, 1, "", sc.failedList)
	for _, c := range rc.Candidates {
		if c.BlockID == failedBlockID {
			t.Fatalf("failed candidate %s must not survive in the round record", failedBlockID)
		}
	}

	// 此前已被候选引用的交易继续等待投票，完整内容不被改写，不带确认/替换关联。
	for _, tx := range []*Transaction{sc.a1, sc.b1} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusProposed {
			t.Fatalf("tx %q status = %s, want proposed (still referenced by an existing candidate)", tx.Content, info.Status)
		}
		assertTxMatches(t, info.Tx, tx)
		if info.BlockHeight != 0 || info.BlockID != "" || info.ReplacedBy != "" {
			t.Fatalf("tx %q must not carry block/replace links: %+v", tx.Content, info)
		}
	}
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{sc.a1.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{sc.b1.ID(): "waiting-vote"})
}

// assertRegisterRolledBack 在 sceneCandidates 之上补充失败候选首次引用的
// 排队交易 c1 的操作前状态：仍排队、内容完整、账户说明等待打包；失败登记
// 不推进任何账户的已确认序号，也不制造序号缺口。
func assertRegisterRolledBack(t *testing.T, n *Node, sc registerScene, keys []testKey) {
	t.Helper()
	sceneCandidates(t, n, sc, keys)

	info := n.mustTx(t, sc.c1.ID())
	if info.Status != StatusQueued {
		t.Fatalf("newly referenced tx c1 status = %s, want queued after the failed register", info.Status)
	}
	assertTxMatches(t, info.Tx, sc.c1)
	if info.BlockHeight != 0 || info.BlockID != "" || info.ReplacedBy != "" {
		t.Fatalf("c1 must not carry block/replace links: %+v", info)
	}
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{sc.c1.ID(): "waiting-pack"})
	if a := n.Account(keys[2].pub); a.Gap != 0 {
		t.Fatalf("account 2 gap = %d, want 0", a.Gap)
	}
}

// assertReplacementStillLocked 尝试以更高费用替换此前已被候选引用的交易，
// 必须继续以 tx-in-proposal 拒绝，且交易与账户状态保持不变。
func assertReplacementStillLocked(t *testing.T, n *Node, keys []testKey, idx int, old *Transaction) {
	t.Helper()
	hi := NewTransaction(keys[idx].priv, old.Sequence, []byte(string(old.Content)+"-hi"), old.Fee+900, 100)
	if _, err := n.Submit(hi); reason(err) != ReasonProposalLocked {
		t.Fatalf("replacing referenced tx %q must still be rejected with %s after the failed register, got %v",
			old.Content, ReasonProposalLocked, err)
	}
	info := n.mustTx(t, old.ID())
	if info.Status != StatusProposed {
		t.Fatalf("referenced tx %q status = %s after rejected replacement, want proposed", old.Content, info.Status)
	}
	assertTxMatches(t, info.Tx, old)
	if info.ReplacedBy != "" {
		t.Fatalf("referenced tx %q must not gain a replacement link: %+v", old.Content, info)
	}
	if a := n.Account(keys[idx].pub); a.ConfirmedSequence != 0 {
		t.Fatalf("account %d confirmed sequence = %d, want 0", idx, a.ConfirmedSequence)
	}
}

// TestRegisterCandidateSaveFailureAtomic 覆盖非空竞争候选登记保存失败的完整
// 回归序列：失败整体回滚（保存错误、结果为 nil，候选/投票/未投票名单/本地
// 提议/两类交易状态/账户序号全部照旧）-> 内存/磁盘/重开三处一致 -> 恢复后
// 首次引用的排队交易可按正常规则加费替换，而已被候选引用的交易仍被锁定。
func TestRegisterCandidateSaveFailureAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 2)
	sc := buildRegisterScene(t, n, keys)

	// 登记前基线：与失败后应当完全一致。
	assertRegisterRolledBack(t, n, sc, keys)

	// 快照登记前的磁盘文件：失败的保存不得改写它，也不得残留临时文件。
	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 合法的非空候选在此刻保存节点状态失败：必须返回保存错误，
	// 且不能同时给出成功的登记结果。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.RegisterCandidate(1, sc.failedList)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("register candidate must return the save error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if res != nil {
		t.Fatalf("register result must be nil on save failure, got %+v", res)
	}

	// 内存：候选、票、未投票名单、本地提议不变；c1 没有被提前锁住，
	// a1、b1 没有被解除保护，账户序号不推进。
	assertRegisterRolledBack(t, n, sc, keys)

	// 磁盘：状态文件逐字节不变，目录中仅有 state.json。
	assertStateFileUnchanged(t, dir, before)

	// 在执行任何后续替换之前重开节点：必须得到与失败后原节点相同的
	// 候选、投票与交易状态，不能恢复出未成功登记的候选或它造成的锁定。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRegisterRolledBack(t, reopened, sc, keys)

	// 保存恢复正常后，失败候选首次引用的排队交易 c1 仍可按原规则加费替换：
	// 同发送者同序号、费用严格更高，替换成功并关联旧标识。
	c1hi := NewTransaction(keys[2].priv, 1, []byte("c1-hi"), 9, 100)
	if c1hi.ID() == sc.c1.ID() || c1hi.Fee <= sc.c1.Fee {
		t.Fatal("setup: replacement must differ from c1 with a strictly higher fee")
	}
	rep, err := reopened.Submit(c1hi)
	if err != nil {
		t.Fatalf("queued tx c1 must stay replaceable after the failed register: %v", err)
	}
	if rep.TxID != c1hi.ID() || rep.ReplacedID != sc.c1.ID() || rep.EvictedID != "" {
		t.Fatalf("replacement result = %+v, want {TxID:%s ReplacedID:%s EvictedID:\"\"}",
			rep, c1hi.ID(), sc.c1.ID())
	}
	old := reopened.mustTx(t, sc.c1.ID())
	if old.Status != StatusReplaced || old.ReplacedBy != c1hi.ID() {
		t.Fatalf("c1 after replacement = {status:%s replacedBy:%s}, want replaced by %s",
			old.Status, old.ReplacedBy, c1hi.ID())
	}
	assertTxMatches(t, old.Tx, sc.c1) // 被替换的旧交易保留完整内容
	next := reopened.mustTx(t, c1hi.ID())
	if next.Status != StatusQueued {
		t.Fatalf("replacement tx status = %s, want queued", next.Status)
	}
	assertTxMatches(t, next.Tx, c1hi)
	assertAccountQueue(t, reopened, keys[2].pub, 0, map[string]string{c1hi.ID(): "waiting-pack"})

	// 候选与投票、轮次与确认高度不被这笔替换改变；a1、b1 仍是等待投票。
	sceneCandidates(t, reopened, sc, keys)

	// 原先已被其他候选引用的交易继续受到保护：加费替换仍以现有的锁定
	// 原因拒绝，不能因为新候选登记失败而被解锁。
	assertReplacementStillLocked(t, reopened, keys, 0, sc.a1)
	assertReplacementStillLocked(t, reopened, keys, 1, sc.b1)

	// 替换与拒绝均已正常落盘：再次重开看到同样的结果——c1 已被替换、
	// c1hi 排队等待打包，a1、b1 仍等待投票，候选与票数不变。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sceneCandidates(t, durable, sc, keys)
	dOld := durable.mustTx(t, sc.c1.ID())
	if dOld.Status != StatusReplaced || dOld.ReplacedBy != c1hi.ID() {
		t.Fatalf("durable c1 = {status:%s replacedBy:%s}, want replaced by %s",
			dOld.Status, dOld.ReplacedBy, c1hi.ID())
	}
	assertTxMatches(t, dOld.Tx, sc.c1)
	dNew := durable.mustTx(t, c1hi.ID())
	if dNew.Status != StatusQueued {
		t.Fatalf("durable replacement tx status = %s, want queued", dNew.Status)
	}
	assertAccountQueue(t, durable, keys[2].pub, 0, map[string]string{c1hi.ID(): "waiting-pack"})

	// c1 既已退出池，原来的候选列表不能原样登记：按现有规则以
	// tx-not-in-pool 拒绝，节点状态不变（仍只有两个候选）。
	if _, err := durable.RegisterCandidate(1, sc.failedList); reason(err) != ReasonTxNotInPool {
		t.Fatalf("register list containing replaced c1 must fail with %s, got %v", ReasonTxNotInPool, err)
	}
	sceneCandidates(t, durable, sc, keys)
}

// assertReRegistered 断言保存恢复后重新提交原合法候选列表成功后的状态：
// 新候选入册（0 票、pending、给定交易顺序），c1 才改为等待投票；原有候选
// 与票数、未投票名单不变，本地提议冻结，轮次与确认高度不前进、不确认区块。
func assertReRegistered(t *testing.T, n *Node, sc registerScene, newID string, keys []testKey) {
	t.Helper()
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0 (registering a competitor must not confirm)",
			n.CurrentRound(), n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("registering a competitor must not create a confirmed block")
	}
	p, ok := n.Proposal()
	if !ok || p.BlockID != sc.local.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(sc.local.TxIDs) {
		t.Fatalf("local proposal must stay frozen: %+v ok=%v, want %+v", p, ok, sc.local)
	}

	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || !rc.HasRecords || len(rc.Candidates) != 3 {
		t.Fatalf("round 1 must have exactly three pending candidates: %+v", rc)
	}
	wantUnvoted := [][]byte{keys[0].pub, keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
	}

	var gotLocal, gotAlt, gotNew *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case sc.local.BlockID:
			gotLocal = &rc.Candidates[i]
		case sc.alt1ID:
			gotAlt = &rc.Candidates[i]
		case newID:
			gotNew = &rc.Candidates[i]
		}
	}
	if gotLocal == nil || gotAlt == nil || gotNew == nil {
		t.Fatalf("all three candidates must be present: %+v", rc.Candidates)
	}
	// 新候选被识别为一个独立的新候选，与本地提议、alt1 标识都不同，0 票，
	// 且不被标记为本地提议。
	if gotNew.Local || gotNew.Result != CandidatePending || len(gotNew.Voters) != 0 ||
		fmt.Sprint(gotNew.TxIDs) != fmt.Sprint(sc.failedList) {
		t.Fatalf("new candidate must be a non-local pending candidate with no votes and the original list: %+v", gotNew)
	}
	// 原有票数仍归属于原来的候选，没有转给新候选。
	if !gotLocal.Local || len(gotLocal.Voters) != 0 {
		t.Fatalf("local candidate votes changed after re-register: %+v", gotLocal)
	}
	wantAltVoters := [][]byte{keys[1].pub, keys[2].pub}
	if fmt.Sprint(hexKeys(gotAlt.Voters)) != fmt.Sprint(hexKeys(wantAltVoters)) {
		t.Fatalf("existing competitor votes = %x, want %x", gotAlt.Voters, wantAltVoters)
	}

	// 届时新引用的排队交易 c1 才改为等待投票并进入账户的等待投票说明。
	c1 := n.mustTx(t, sc.c1.ID())
	if c1.Status != StatusProposed {
		t.Fatalf("c1 status = %s, want proposed only after the successful register", c1.Status)
	}
	assertTxMatches(t, c1.Tx, sc.c1)
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{sc.c1.ID(): "waiting-vote"})
	// 已确认序号仍不推进。
	for i := 0; i < 3; i++ {
		if a := n.Account(keys[i].pub); a.ConfirmedSequence != 0 {
			t.Fatalf("account %d confirmed sequence = %d, want 0", i, a.ConfirmedSequence)
		}
	}

	// c1 现已被未决候选引用：加费替换按现有锁定原因拒绝。
	c1hi := NewTransaction(keys[2].priv, 1, []byte("c1-hi-late"), 9, 100)
	if _, err := n.Submit(c1hi); reason(err) != ReasonProposalLocked {
		t.Fatalf("c1 must be locked after the successful register, got %v (want %s)", err, ReasonProposalLocked)
	}
	if info := n.mustTx(t, sc.c1.ID()); info.Status != StatusProposed || info.ReplacedBy != "" {
		t.Fatalf("c1 must stay proposed with no replacement link after rejected replace: %+v", info)
	}
}

// TestRegisterCandidateSaveFailureRetrySucceeds 钉住恢复路径：保存失败并回滚、
// 重新打开节点后，在尚未替换列表中交易的情况下重新提交原来的合法候选列表，
// 必须成功登记并被识别为新候选（而非重复登记的既有候选），新引用的排队交易
// 才改为等待投票，原有投票归属不变，本地提议保持冻结、不直接确认区块。
func TestRegisterCandidateSaveFailureRetrySucceeds(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 2)
	sc := buildRegisterScene(t, n, keys)

	wantNewID := BlockID(1, 1, "", sc.failedList)
	if wantNewID == sc.local.BlockID || wantNewID == sc.alt1ID {
		t.Fatal("setup: retried candidate must differ from both existing candidates")
	}

	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 合法登记保存失败：整体回滚，内存/磁盘/重开三处一致。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.RegisterCandidate(1, sc.failedList)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("register candidate must return the save error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if res != nil {
		t.Fatalf("register result must be nil on save failure, got %+v", res)
	}
	assertRegisterRolledBack(t, n, sc, keys)
	assertStateFileUnchanged(t, dir, before)

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRegisterRolledBack(t, reopened, sc, keys)

	// 保存恢复正常，且尚未替换列表中的任何交易：重新提交原列表必须成功，
	// 并被识别为一个新候选（Existing=false），而非重复登记。
	res, err = reopened.RegisterCandidate(1, append([]string{}, sc.failedList...))
	if err != nil {
		t.Fatalf("retry of the original valid list must succeed after recovery: %v", err)
	}
	if res.Existing {
		t.Fatal("the retried candidate must be registered as a new candidate, not an existing one")
	}
	if res.BlockID != wantNewID {
		t.Fatalf("retried block id = %s, want %s", res.BlockID, wantNewID)
	}
	assertReRegistered(t, reopened, sc, wantNewID, keys)

	// 成功登记随再次落盘持久化：再开一次看到完全一致的三候选、票数与锁定状态。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertReRegistered(t, durable, sc, wantNewID, keys)
}
