package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住“竞争候选登记”的原子落盘回归保障。
//
// 场景：第 1 轮本地提议已产生、当前轮次尚未确认，一个已登记的竞争候选保留了
// 一张未达确认门槛的投票（四验证者门槛为 3 票）。准备登记的新竞争候选为非空
// 列表，其中既包含已被现有候选引用而处于等待投票的交易，也包含此前只在池中
// 排队、首次被候选引用的交易；列表满足单块上限与各账户序号自已确认序号加一
// 起连续的规则，各笔交易均未到期。登记校验全部通过后，如果这次状态保存发生
// 错误：
//
//	- 调用者必须收到保存错误，不能得到成功的登记结果（结果为 nil，而非
//	  RegisterResult，也不是某种业务拒绝原因）；
//	- 按轮次查询仍只列出原来的候选，本地提议的区块标识与交易顺序、各候选的
//	  投票者及未投票名单都保持登记前的结果，轮次与确认高度不前进；
//	- 失败请求中首次被候选引用的排队交易仍显示排队、账户查询仍说明等待打包，
//	  完整内容不变；同发送者同序号、费用严格更高的新交易仍可替换它，旧交易
//	  随后按原规则显示已被替换并关联新交易标识；
//	- 原先已被其他候选引用、本次只是共享引用的交易继续等待投票，加费替换仍
//	  以 tx-in-proposal 拒绝，不能因为新候选登记失败而被解除保护；
//	- 登记失败本身不应改写这两类交易的完整内容（发送者、序号、内容、费用、
//	  到期轮次、签名逐字段不变），也不应推进账户已确认序号；
//	- 在执行后续替换之前重新打开该节点的状态目录，应得到与失败后原节点相同
//	  的候选、投票和交易状态，不能恢复出未成功登记的候选或它造成的交易锁定。
//
// 保存恢复正常后分两条互斥的回归支线，分别用两个节点验证：
//
//	- 先替换：新引用的排队交易可被正常加费替换，而已锁定的共享交易继续被拒；
//	- 不替换、重新提交原来的合法候选列表：应能成功登记并被识别为一个全新候选
//	  （而非失败尝试的残留或“重复登记”）；届时新引用的排队交易才改为等待投票，
//	  原有票数仍归属于原来的候选，本地提议保持冻结，票数不转移、区块不确认。
//
// 本组测试不新增公开入口，也不改变现有登记、投票和交易替换的公开行为。

// regScene 汇总第 1 轮未决场景中的关键视图与交易，供失败后各处断言复用。
type regScene struct {
	local   ProposalView
	oldAlt  string       // 已存在竞争候选的区块标识（持有 1 张未达门槛的票）
	failID  string       // 保存失败的登记本应产生的区块标识
	shared  *Transaction // 账户0：本地提议 + 现有竞争候选共享引用，等待投票
	inLocal *Transaction // 账户3：仅本地提议引用，等待投票
	fresh   *Transaction // 账户1：上限外排队，失败候选首次引用
	queued  *Transaction // 账户2：候选外排队，全程不被任何候选引用（对照）
}

// regFailList 是保存失败（及恢复后重试）的竞争候选交易顺序：非空、2 笔、
// 不超过单块上限 2，每个账户序号都自已确认序号 0 加一开始连续。
func regFailList(s *regScene) []string {
	return []string{s.fresh.ID(), s.shared.ID()}
}

// buildRegisterFailureScene 在四验证者、单块上限 2 的节点上构造登记失败前的
// 未决场景：
//
//	shared  账户0 seq1 fee10 本地提议 + 现有竞争候选 -> 等待投票
//	inLocal 账户3 seq1 fee8  仅本地提议（本地上限取费用前二）-> 等待投票
//	fresh   账户1 seq1 fee5  上限外，仅池内排队 -> 等待打包，将被失败候选首次引用
//	queued  账户2 seq1 fee1  上限外，候选外排队（对照）-> 等待打包
//
// 现有竞争候选只引用 shared，验证者1 给它一票（1/3，未达门槛）。
func buildRegisterFailureScene(t *testing.T, n *Node, keys []testKey) *regScene {
	t.Helper()
	s := &regScene{
		shared:  NewTransaction(keys[0].priv, 1, []byte("shared"), 10, 100),
		inLocal: NewTransaction(keys[3].priv, 1, []byte("in-local"), 8, 100),
		fresh:   NewTransaction(keys[1].priv, 1, []byte("fresh"), 5, 100),
		queued:  NewTransaction(keys[2].priv, 1, []byte("queued"), 1, 100),
	}
	for _, tx := range []*Transaction{s.shared, s.inLocal, s.fresh, s.queued} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}
	// 单块上限 2：本地提议按费用取 shared(10)、inLocal(8)；
	// fresh(5)、queued(1) 留在池中排队。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	wantLocal := []string{s.shared.ID(), s.inLocal.ID()}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint(wantLocal) {
		t.Fatalf("setup: local proposal = %v, want %v", local.TxIDs, wantLocal)
	}
	s.local = local

	// 现有竞争候选只引用 shared，使 shared 先被现有候选锁定；验证者1 投一票，
	// 未达四验证者的 3 票门槛。
	alt, err := n.RegisterCandidate(1, []string{s.shared.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt.Existing || alt.BlockID == local.BlockID {
		t.Fatalf("setup: existing competitor must be a distinct new candidate: %+v", alt)
	}
	s.oldAlt = alt.BlockID
	if res, err := n.Vote(keys[1].pub, 1, alt.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("setup: sub-threshold competitor vote: %+v %v", res, err)
	}

	s.failID = BlockID(1, 1, "", regFailList(s))
	if s.failID == local.BlockID || s.failID == alt.BlockID {
		t.Fatal("setup: failing candidate id must differ from existing candidates")
	}
	return s
}

// assertSceneUnchanged 断言轮次/确认高度、本轮候选集合与票数、未投票名单以及
// 冻结的本地提议完全保持“登记前/失败后”的结果：本轮恰好两个候选，失败候选的
// 区块标识不存在；本地候选保持冻结的标识与交易顺序、0 票，竞争候选保留
// 验证者1 的那一票；未投票名单为验证者0/2/3（按公钥排序）；轮次不前进、
// 不产生确认块。不涉及交易与账户状态（替换支线会另行断言）。
func assertSceneUnchanged(t *testing.T, n *Node, keys []testKey, s *regScene) {
	t.Helper()
	if n.CurrentRound() != 1 {
		t.Fatalf("current round = %d, want 1 (failed register must not advance the round)", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0 (failed register must not confirm a block)", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("no confirmed block must exist while the round is pending")
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
	var gotLocal, gotOld *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case s.failID:
			t.Fatalf("failed candidate %s must not appear in the round record", s.failID)
		case s.local.BlockID:
			gotLocal = &rc.Candidates[i]
		case s.oldAlt:
			gotOld = &rc.Candidates[i]
		default:
			t.Fatalf("unexpected candidate %s", rc.Candidates[i].BlockID)
		}
	}
	if gotLocal == nil || gotOld == nil {
		t.Fatalf("both original candidates must remain present: %+v", rc.Candidates)
	}
	// 本地提议：冻结的标识与交易顺序，0 票，仍标记 Local。
	wantLocalOrder := []string{s.shared.ID(), s.inLocal.ID()}
	if !gotLocal.Local || gotLocal.Result != CandidatePending ||
		gotLocal.BlockID != s.local.BlockID || fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint(wantLocalOrder) ||
		len(gotLocal.Voters) != 0 {
		t.Fatalf("local candidate must stay frozen with its order and no votes: %+v", gotLocal)
	}
	// 现有竞争候选：pending、只有 shared，保留验证者1 的那一票。
	if gotOld.Local || gotOld.Result != CandidatePending ||
		fmt.Sprint(gotOld.TxIDs) != fmt.Sprint([]string{s.shared.ID()}) ||
		len(gotOld.Voters) != 1 || fmt.Sprintf("%x", gotOld.Voters[0]) != fmt.Sprintf("%x", keys[1].pub) {
		t.Fatalf("existing competitor must keep its single original vote: %+v", gotOld)
	}
	// 未投票名单：验证者0、2、3，按公钥十六进制排序。
	wantUnvoted := [][]byte{keys[0].pub, keys[2].pub, keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
	}

	// Proposal 始终返回同一份冻结的本地提议。
	p, ok := n.Proposal()
	if !ok || p.Round != 1 || p.BlockID != s.local.BlockID ||
		fmt.Sprint(p.TxIDs) != fmt.Sprint(wantLocalOrder) {
		t.Fatalf("local proposal changed after the failed register: %+v ok=%v, want id=%s order=%v",
			p, ok, s.local.BlockID, wantLocalOrder)
	}
}

// assertTxsPreRegister 断言登记前/失败后的交易与账户状态：shared/inLocal 等待
// 投票，fresh/queued 排队等待打包，均无关联、内容完整；账户已确认序号均为 0。
func assertTxsPreRegister(t *testing.T, n *Node, keys []testKey, s *regScene) {
	t.Helper()
	assertLockedTxIntact(t, n, s.shared)
	assertLockedTxIntact(t, n, s.inLocal)
	assertQueuedTxIntact(t, n, s.fresh)
	assertQueuedTxIntact(t, n, s.queued)
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{s.shared.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[3].pub, 0, map[string]string{s.inLocal.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{s.fresh.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{s.queued.ID(): "waiting-pack"})
}

// assertRegisterScenePending 断言节点完全处于“失败登记前/失败后”的未决状态：
// 候选/投票/提议格局不变，且两类交易状态与账户序号保持登记前结果。
func assertRegisterScenePending(t *testing.T, n *Node, keys []testKey, s *regScene) {
	t.Helper()
	assertSceneUnchanged(t, n, keys, s)
	assertTxsPreRegister(t, n, keys, s)
}

// assertQueuedTxIntact 断言一笔交易仍是排队等待打包状态，无任何区块/替换/淘汰
// 关联，且完整内容与原交易逐字段一致。
func assertQueuedTxIntact(t *testing.T, n *Node, tx *Transaction) {
	t.Helper()
	info := n.mustTx(t, tx.ID())
	if info.Status != StatusQueued {
		t.Fatalf("tx %q status = %s, want queued", tx.Content, info.Status)
	}
	assertTxMatches(t, info.Tx, tx)
	if info.ReplacedBy != "" || info.BlockHeight != 0 || info.BlockID != "" ||
		info.DropReason != "" || info.DropRound != 0 {
		t.Fatalf("queued tx %q must carry no links: %+v", tx.Content, info)
	}
}

// assertLockedTxIntact 断言一笔交易仍处于等待投票状态，无任何区块/替换/淘汰
// 关联，且完整内容与原交易逐字段一致。
func assertLockedTxIntact(t *testing.T, n *Node, tx *Transaction) {
	t.Helper()
	info := n.mustTx(t, tx.ID())
	if info.Status != StatusProposed {
		t.Fatalf("tx %q status = %s, want proposed (waiting for votes)", tx.Content, info.Status)
	}
	assertTxMatches(t, info.Tx, tx)
	if info.ReplacedBy != "" || info.BlockHeight != 0 || info.BlockID != "" ||
		info.DropReason != "" || info.DropRound != 0 {
		t.Fatalf("locked tx %q must carry no links: %+v", tx.Content, info)
	}
}

// TestRegisterCandidateSaveFailureAtomic 覆盖支线一：非空竞争候选登记保存失败
// 后整体回滚（保存错误、结果为 nil；候选、投票、未投票名单、本地提议、两类
// 交易状态与内容、账户序号全部照旧）-> 内存/磁盘/重开三处一致（重开发生在
// 任何替换之前）-> 恢复后首次被引用的排队交易可按原规则加费替换，而已被其他
// 候选引用的共享交易继续以 tx-in-proposal 拒绝，失败候选始终不存在。
func TestRegisterCandidateSaveFailureAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 2)
	s := buildRegisterFailureScene(t, n, keys)
	assertRegisterScenePending(t, n, keys, s)

	// 快照登记前的磁盘文件：失败的保存既不得改写它，也不得残留临时文件。
	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 合法的非空候选在此刻保存节点状态失败：必须返回保存错误，且不能同时
	// 给出成功的登记结果。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.RegisterCandidate(1, regFailList(s))
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("competitor registration must return the save error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if res != nil {
		t.Fatalf("register result must be nil on save failure, got %+v", res)
	}

	// 内存：候选、票、未投票名单、本地提议、两类交易与账户查询全部保持登记前结果。
	assertRegisterScenePending(t, n, keys, s)

	// 磁盘：状态文件逐字节不变，目录中仅有 state.json。
	assertStateFileUnchanged(t, dir, before)

	// 在执行后续替换之前重开节点：与失败后原节点完全相同的候选、投票与交易
	// 状态，不能恢复出未成功登记的候选或它造成的交易锁定。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRegisterScenePending(t, reopened, keys, s)

	// 保存恢复正常：同发送者同序号、费用严格更高的新交易仍可替换首次被引用、
	// 实际仍在排队的 fresh。
	freshHi := NewTransaction(keys[1].priv, 1, []byte("fresh-hi"), 50, 100)
	if freshHi.ID() == s.fresh.ID() || freshHi.Fee <= s.fresh.Fee {
		t.Fatal("setup: replacement must differ from fresh with a strictly higher fee")
	}
	rep, err := reopened.Submit(freshHi)
	if err != nil {
		t.Fatalf("queued tx must stay replaceable after the failed registration: %v", err)
	}
	if rep.TxID != freshHi.ID() || rep.ReplacedID != s.fresh.ID() || rep.EvictedID != "" {
		t.Fatalf("replacement result = %+v, want {TxID:%s ReplacedID:%s EvictedID:\"\"}",
			rep, freshHi.ID(), s.fresh.ID())
	}
	// 旧交易按原规则显示已被替换并关联新交易标识，完整内容保留；新交易排队
	// 等待打包，账户已确认序号仍为 0。
	oldInfo := reopened.mustTx(t, s.fresh.ID())
	if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != freshHi.ID() {
		t.Fatalf("fresh = {status:%s replacedBy:%s}, want replaced by %s",
			oldInfo.Status, oldInfo.ReplacedBy, freshHi.ID())
	}
	assertTxMatches(t, oldInfo.Tx, s.fresh)
	assertQueuedTxIntact(t, reopened, freshHi)
	assertAccountQueue(t, reopened, keys[1].pub, 0, map[string]string{freshHi.ID(): "waiting-pack"})

	// 原先已被其他候选引用的共享交易继续受保护：加费替换仍以 tx-in-proposal
	// 拒绝，状态与内容不变，没有替代关联。
	sharedHi := NewTransaction(keys[0].priv, 1, []byte("shared-hi"), 99, 100)
	if _, err := reopened.Submit(sharedHi); reason(err) != ReasonProposalLocked {
		t.Fatalf("replacing the shared locked tx got %v, want %s", err, ReasonProposalLocked)
	}
	assertLockedTxIntact(t, reopened, s.shared)
	if _, err := reopened.Tx(sharedHi.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected replacement tx must leave no record, got %v", err)
	}

	// 候选与投票格局不因替换改变：仍只有两个原候选，失败候选不存在，
	// 轮次与确认高度不前进；其余三笔交易（含对照的 queued 与两笔锁定交易）
	// 状态与内容保持登记前结果，账户已确认序号仍为 0。
	assertSceneUnchanged(t, reopened, keys, s)
	assertLockedTxIntact(t, reopened, s.shared)
	assertLockedTxIntact(t, reopened, s.inLocal)
	assertQueuedTxIntact(t, reopened, s.queued)
	assertAccountQueue(t, reopened, keys[0].pub, 0, map[string]string{s.shared.ID(): "waiting-vote"})
	assertAccountQueue(t, reopened, keys[3].pub, 0, map[string]string{s.inLocal.ID(): "waiting-vote"})
	assertAccountQueue(t, reopened, keys[2].pub, 0, map[string]string{s.queued.ID(): "waiting-pack"})

	// 替换结果随再次落盘持久化：重开后 fresh 仍为 replaced 并指向 freshHi，
	// shared 仍等待投票，失败候选仍不存在。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	dInfo := durable.mustTx(t, s.fresh.ID())
	if dInfo.Status != StatusReplaced || dInfo.ReplacedBy != freshHi.ID() {
		t.Fatalf("durable replaced state wrong: %+v", dInfo)
	}
	dHi := durable.mustTx(t, freshHi.ID())
	if dHi.Status != StatusQueued {
		t.Fatalf("replacement tx status = %s, want queued", dHi.Status)
	}
	assertLockedTxIntact(t, durable, s.shared)
	rc, err := durable.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("durable candidate count = %d, want 2", len(rc.Candidates))
	}
	for _, c := range rc.Candidates {
		if c.BlockID == s.failID {
			t.Fatal("failed candidate must never become durable")
		}
	}
}

// TestRegisterCandidateSaveFailureRetryAfterRecovery 覆盖支线二：保存恢复正常
// 后，在尚未替换列表中交易的节点上重新提交原来的合法候选列表——必须成功登记
// 并被识别为一个全新候选（区块标识与失败尝试本应产生的相同，但 Existing 为
// false）；届时新引用的排队交易才改为等待投票，原有票数仍归属于原来的候选，
// 本地提议保持冻结，票数不转移、区块不确认；结果随再次落盘持久化。
func TestRegisterCandidateSaveFailureRetryAfterRecovery(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 2)
	s := buildRegisterFailureScene(t, n, keys)

	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.RegisterCandidate(1, regFailList(s))
	n.injectSaveErr = nil
	if err == nil || res != nil {
		t.Fatalf("failing register must return error/nil, got %+v / %v", res, err)
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, got rejection %q: %v", reason(err), err)
	}
	assertRegisterScenePending(t, n, keys, s)
	assertStateFileUnchanged(t, dir, before)

	// 不做任何替换，重开后重新提交同一份合法列表。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRegisterScenePending(t, reopened, keys, s)

	list := regFailList(s)
	r2, err := reopened.RegisterCandidate(1, append([]string{}, list...))
	if err != nil {
		t.Fatalf("re-registering the valid list after recovery must succeed: %v", err)
	}
	// 这是一个全新候选，不是失败尝试的残留，也不是“相同列表重复登记”：
	// 标识与失败尝试本应产生的相同，但此前从未保存过。
	if r2.Existing {
		t.Fatalf("retried registration must be recognized as a new candidate, got Existing=true: %+v", r2)
	}
	if r2.BlockID != s.failID {
		t.Fatalf("retried candidate id = %s, want the deterministic id %s", r2.BlockID, s.failID)
	}

	// 成功登记后：三个候选均 pending；本地提议冻结，现有竞争候选保留原票，
	// 新候选 0 票；未投票名单不变；轮次与确认高度不前进、区块未确认。
	rc, err := reopened.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || len(rc.Candidates) != 3 {
		t.Fatalf("round 1 must have three pending candidates after retry: %+v", rc)
	}
	var local, oldAlt, newAlt *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case s.local.BlockID:
			local = &rc.Candidates[i]
		case s.oldAlt:
			oldAlt = &rc.Candidates[i]
		case s.failID:
			newAlt = &rc.Candidates[i]
		}
	}
	if local == nil || oldAlt == nil || newAlt == nil {
		t.Fatalf("all three candidates must be present: %+v", rc.Candidates)
	}
	if !local.Local || len(local.Voters) != 0 ||
		fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{s.shared.ID(), s.inLocal.ID()}) {
		t.Fatalf("local proposal must stay frozen with no votes: %+v", local)
	}
	if len(oldAlt.Voters) != 1 || fmt.Sprintf("%x", oldAlt.Voters[0]) != fmt.Sprintf("%x", keys[1].pub) {
		t.Fatalf("original competitor must keep its original vote (no transfer): %+v", oldAlt)
	}
	if newAlt.Local || len(newAlt.Voters) != 0 ||
		fmt.Sprint(newAlt.TxIDs) != fmt.Sprint(list) || newAlt.Result != CandidatePending {
		t.Fatalf("new candidate must be pending with no transferred votes: %+v", newAlt)
	}
	wantUnvoted := [][]byte{keys[0].pub, keys[2].pub, keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted list changed after retry: %x", rc.Unvoted)
	}
	if reopened.CurrentRound() != 1 || reopened.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0 (registration only adds a choice)", reopened.CurrentRound(), reopened.Height())
	}

	// 届时新引用的排队交易才改为等待投票；共享交易、本地上限内交易继续等待
	// 投票，候选外对照交易仍排队；账户已确认序号不推进。
	assertLockedTxIntact(t, reopened, s.fresh)
	assertLockedTxIntact(t, reopened, s.shared)
	assertLockedTxIntact(t, reopened, s.inLocal)
	assertQueuedTxIntact(t, reopened, s.queued)
	assertAccountQueue(t, reopened, keys[1].pub, 0, map[string]string{s.fresh.ID(): "waiting-vote"})
	assertAccountQueue(t, reopened, keys[0].pub, 0, map[string]string{s.shared.ID(): "waiting-vote"})
	assertAccountQueue(t, reopened, keys[3].pub, 0, map[string]string{s.inLocal.ID(): "waiting-vote"})
	assertAccountQueue(t, reopened, keys[2].pub, 0, map[string]string{s.queued.ID(): "waiting-pack"})

	// 新登记只增加竞争选择：fresh 现在也被锁定，加费替换被拒；给新候选投一票
	// 只计一票、不会直接确认区块；验证者1 不能把原票改投给新候选。
	freshHi := NewTransaction(keys[1].priv, 1, []byte("fresh-hi"), 50, 100)
	if _, err := reopened.Submit(freshHi); reason(err) != ReasonProposalLocked {
		t.Fatalf("fresh must be locked after the successful retry, got %v", err)
	}
	if _, err := reopened.Vote(keys[1].pub, 1, s.failID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("original voter must not switch to the new candidate, got %v", err)
	}
	v, err := reopened.Vote(keys[0].pub, 1, s.failID)
	if err != nil || !v.Counted || v.Confirmed {
		t.Fatalf("first vote for the new candidate must count without confirming: %+v %v", v, err)
	}
	if reopened.CurrentRound() != 1 || reopened.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0 (one vote must not confirm)", reopened.CurrentRound(), reopened.Height())
	}

	// 成功登记与新票随再次落盘持久化：重开看到三候选、票数归属与交易状态一致。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	drc, err := durable.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(drc.Candidates) != 3 {
		t.Fatalf("durable candidate count = %d, want 3", len(drc.Candidates))
	}
	votesByID := map[string]int{}
	for _, c := range drc.Candidates {
		votesByID[c.BlockID] = len(c.Voters)
	}
	if votesByID[s.local.BlockID] != 0 || votesByID[s.oldAlt] != 1 || votesByID[s.failID] != 1 {
		t.Fatalf("durable votes local/old/new = %d/%d/%d, want 0/1/1",
			votesByID[s.local.BlockID], votesByID[s.oldAlt], votesByID[s.failID])
	}
	if p, ok := durable.Proposal(); !ok || p.BlockID != s.local.BlockID {
		t.Fatalf("durable local proposal changed: %+v ok=%v", p, ok)
	}
	assertLockedTxIntact(t, durable, s.fresh)
	assertQueuedTxIntact(t, durable, s.queued)
}
