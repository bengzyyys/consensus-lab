package consensus

import (
	"fmt"
	"testing"
)

// 本文件钉住竞争候选登记入口的“登记输入独立性”：RegisterCandidate 成功后，
// 调用方继续复用并编辑当初传入的交易标识列表，已登记候选必须保持登记成功时的
// 区块标识、完整交易列表与次序，既不随调用方交换位置而改序，也不因调用方把某个
// 位置改成另一笔交易（池内有效交易、未知标识或重复标识）而改变内容或候选集合。
// 这与 submit_input_copy_test.go（提交入口拷贝）和
// candidates_query_copy_regression_test.go（查询出口拷贝）分别守住另一侧：
// 登记入口必须在成功时取得列表快照。
//
// 固定场景：四名验证者、单块上限 4、交易池不限容量。第 1 轮本地提议前提交
// a1（账户 A 序号 1，费用 5）与 b1（账户 B 序号 1，费用 9），本地提议按费用
// 排序为 [b1, a1]；提议冻结后再提交 a2（账户 A 序号 2，费用 1），它只在池中
// 排队、不被任何候选引用。随后调用方以与费用排序相反的交错顺序 [a1, b1]
// （两个账户、序号各自从已确认序号 0 加一）登记竞争候选，登记后立即在原列表
// 上做编辑，再由名单内验证者向原区块标识投票。该候选登记时的标识即
// BlockID(1, 1, "", [a1, b1])。
//
// 本组测试不新增公开入口，也不改变现有登记、投票、确认与查询规则。

// registerSnapshotScene 收拢“登记输入独立性”场景的关键句柄。
type registerSnapshotScene struct {
	n     *Node
	keys  []testKey
	local ProposalView
	altID string
	// a1、b1 被本地提议与竞争候选引用：等待投票。
	a1 *Transaction
	b1 *Transaction
	// a2 在提议冻结后才提交，仅在池中排队，从未被任何候选引用。
	a2 *Transaction
	// list 是调用方登记竞争候选时传入、登记成功后继续持有的原列表。
	list []string
}

// buildRegisterSnapshotScene 构造第 1 轮未决场景：本地提议 [b1, a1]，
// a2 仅排队；竞争候选 [a1, b1] 已登记但尚无投票。投票由各测试在编辑列表之后
// 显式投出，以便“登记成功后立即编辑原列表”的断言真正作用于登记入口取得的快照，
// 而不是被后续写操作的内部深拷贝兜底。
func buildRegisterSnapshotScene(t *testing.T) registerSnapshotScene {
	t.Helper()
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4) // 单块上限 4，池容量 0 表示不限制

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 5, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 9, 100)
	for _, tx := range []*Transaction{a1, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}

	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	// 打包按费用：b1(9) 在 a1(5) 之前。
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{b1.ID(), a1.ID()}) {
		t.Fatalf("setup: local proposal = %v, want [b1 a1] by fee", local.TxIDs)
	}

	// 提议冻结后才提交的 a2 只在池中排队，不被本地提议引用。
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 1, 100)
	if _, err := n.Submit(a2); err != nil {
		t.Fatal(err)
	}
	if info := n.mustTx(t, a2.ID()); info.Status != StatusQueued {
		t.Fatalf("setup: a2 status = %s, want queued", info.Status)
	}

	// 调用方给定的候选顺序与费用排序相反：两账户交错，各自序号从已确认序号加一。
	list := []string{a1.ID(), b1.ID()}
	res, err := n.RegisterCandidate(1, list)
	if err != nil {
		t.Fatalf("setup register contending candidate: %v", err)
	}
	wantID := BlockID(1, 1, "", list)
	if res.Existing || res.BlockID != wantID || res.BlockID == local.BlockID {
		t.Fatalf("setup: register result = %+v, want fresh %s distinct from local %s",
			res, wantID, local.BlockID)
	}

	return registerSnapshotScene{
		n:     n,
		keys:  keys,
		local: local,
		altID: res.BlockID,
		a1:    a1,
		b1:    b1,
		a2:    a2,
		list:  list,
	}
}

// assertOriginalCandidate 断言竞争候选始终保持登记时的内容：区块标识不变，
// 交易列表与次序仍是 [a1, b1]，结果未决，得票恰好为给定验证者（按公钥排序）。
func assertOriginalCandidate(t *testing.T, sc registerSnapshotScene, voters ...[]byte) {
	t.Helper()
	c := findCandidate(t, sc.n, 1, sc.altID)
	wantOrder := []string{sc.a1.ID(), sc.b1.ID()}
	if c.BlockID != sc.altID || c.Local || c.Result != CandidatePending {
		t.Fatalf("contending candidate changed: %+v, want pending id %s", c, sc.altID)
	}
	if fmt.Sprint(c.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("contending candidate tx order = %v, want registered order %v", c.TxIDs, wantOrder)
	}
	if fmt.Sprint(pubHexList(c.Voters)) != fmt.Sprint(sortedPubHex(voters...)) {
		t.Fatalf("contending candidate voters = %x, want %x", c.Voters, voters)
	}
}

// assertRoundKeepsExactlyTwoCandidates 断言轮次仍只有本地提议与原竞争候选，
// 两者保留各自登记时的标识、顺序与结果，本地提议标识不被编辑改变，轮次未决出。
func assertRoundKeepsExactlyTwoCandidates(t *testing.T, sc registerSnapshotScene) {
	t.Helper()
	if sc.n.CurrentRound() != 1 || sc.n.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0 (caller list edits must not advance anything)",
			sc.n.CurrentRound(), sc.n.Height())
	}
	rc, err := sc.n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords || len(rc.Candidates) != 2 {
		t.Fatalf("round 1 must keep exactly the two registered candidates: %+v", rc)
	}
	got := map[string]*CandidateView{}
	for i := range rc.Candidates {
		got[rc.Candidates[i].BlockID] = &rc.Candidates[i]
	}
	alt, ok := got[sc.altID]
	if !ok {
		t.Fatalf("original contending candidate missing: %+v", rc.Candidates)
	}
	if fmt.Sprint(alt.TxIDs) != fmt.Sprint([]string{sc.a1.ID(), sc.b1.ID()}) {
		t.Fatalf("original candidate order = %v, want [a1 b1]", alt.TxIDs)
	}
	local, ok := got[sc.local.BlockID]
	if !ok || !local.Local {
		t.Fatalf("local proposal missing or not marked local: %+v", rc.Candidates)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{sc.b1.ID(), sc.a1.ID()}) {
		t.Fatalf("local proposal order = %v, want [b1 a1]", local.TxIDs)
	}
	// 本地提议的标识与内容也必须是登记时的样子。
	p, ok := sc.n.Proposal()
	if !ok || p.BlockID != sc.local.BlockID ||
		fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{sc.b1.ID(), sc.a1.ID()}) {
		t.Fatalf("local proposal changed: %+v ok=%v, want %+v", p, ok, sc.local)
	}
}

// TestRegisterCandidateCallerListEditsDoNotChangeCandidate 钉住接受路径下的
// 列表快照：登记成功后调用方直接交换原列表两项位置，或把某个位置的标识改成另一笔
// 仍在池内的有效交易，原候选的区块标识、交易列表与次序仍与登记时一致；本地提议
// 不变，候选集合不因编辑而增减，登记也从未推进轮次、确认高度或账户已确认序号。
// 原候选引用的交易继续等待投票；仅被编辑后的列表提到、未被任何候选引用的 a2
// 仍等待打包，账户查询的等待说明与各自状态一致。
func TestRegisterCandidateCallerListEditsDoNotChangeCandidate(t *testing.T) {
	sc := buildRegisterSnapshotScene(t)
	a1ID, b1ID, a2ID := sc.a1.ID(), sc.b1.ID(), sc.a2.ID()

	// 第一类编辑：登记成功后立即（尚未进行任何会触发内部深拷贝的写操作）直接
	// 交换原列表中两个已有项的位置（[a1, b1] -> [b1, a1]）。
	sc.list[0], sc.list[1] = sc.list[1], sc.list[0]
	assertOriginalCandidate(t, sc)
	assertRoundKeepsExactlyTwoCandidates(t, sc)

	// 交换回来后做第二类编辑：把某个位置的标识改成另一笔仍在池内的有效交易 a2，
	// 使调用方手中的列表变成 [a1, a2]。
	sc.list[0], sc.list[1] = sc.list[1], sc.list[0]
	sc.list[1] = a2ID
	assertOriginalCandidate(t, sc)
	assertRoundKeepsExactlyTwoCandidates(t, sc)

	// 编辑列表不会登记出新候选、也不会让原候选改引用 a2：候选集合仍是两个。
	rc, err := sc.n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rc.Candidates {
		for _, id := range c.TxIDs {
			if id == a2ID {
				t.Fatalf("a2 must not be referenced by any candidate after list edit: %+v", c)
			}
		}
	}

	// 编辑之后再投一张合法票：票计入原区块标识、尚不确认，原候选顺序与这张票
	// 都保留，轮次与确认高度仍不推进。
	vres, err := sc.n.Vote(sc.keys[1].pub, 1, sc.altID)
	if err != nil || !vres.Counted || vres.Confirmed {
		t.Fatalf("vote after caller list edits: %+v %v", vres, err)
	}
	assertOriginalCandidate(t, sc, sc.keys[1].pub)
	assertRoundKeepsExactlyTwoCandidates(t, sc)

	// 两类交易的账户查询说明与各自状态一致：原候选引用的 a1、b1 等待投票；
	// 仅被编辑后列表提到的 a2 仍等待打包。账户已确认序号不推进，缺口为 0。
	assertAccountQueue(t, sc.n, sc.keys[0].pub, 0, map[string]string{
		a1ID: "waiting-vote", a2ID: "waiting-pack",
	})
	assertAccountQueue(t, sc.n, sc.keys[1].pub, 0, map[string]string{
		b1ID: "waiting-vote",
	})
	for _, id := range []string{a1ID, b1ID} {
		if info := sc.n.mustTx(t, id); info.Status != StatusProposed {
			t.Fatalf("candidate tx %s status = %s, want proposed (waiting-vote)", id, info.Status)
		}
	}
	if info := sc.n.mustTx(t, a2ID); info.Status != StatusQueued {
		t.Fatalf("edited-in tx %s status = %s, want queued (waiting-pack)", a2ID, info.Status)
	}
}

// TestRegisterCandidateSnapshotSurvivesInvalidEditsVotingAndConfirm 钉住：
// 调用方把原列表改成含未知标识或重复有效标识后，既不重新提交登记，则对原候选的
// 正常查询与合法投票都不能失败或改变确认内容；名单内验证者继续向原区块标识投票，
// 达到严格超过三分之二门槛时，确认返回块与按高度查询到的确认历史都采用原始交易
// 列表与顺序。交易查询关联这个胜出块，各账户只推进到原候选实际包含的最后一个序号，
// 被调用方写进列表的 a2 不会因此得到确认。同一轮尚未决出时把编辑后的列表再次用于
// 登记，仍按既有的 unknown-transaction / duplicate-in-list 原因拒绝，原候选与已有
// 票数保持原样；恢复原列表后重复登记只返回同一候选并保留票数。
func TestRegisterCandidateSnapshotSurvivesInvalidEditsVotingAndConfirm(t *testing.T) {
	sc := buildRegisterSnapshotScene(t)
	a1ID, b1ID, a2ID := sc.a1.ID(), sc.b1.ID(), sc.a2.ID()
	wantOrder := []string{a1ID, b1ID}
	// 一个实际不存在的 64 位十六进制交易标识。
	unknownID := "0000000000000000000000000000000000000000000000000000000000000000"

	// 登记成功后立即（尚未投票、尚未触发任何内部深拷贝）把原列表改成
	// “未知标识 + 有效标识重复”：[unknown, a1, a1]。
	sc.list[0] = unknownID
	sc.list[1] = a1ID
	sc.list = append(sc.list, a1ID)

	// 编辑没有再次提交给登记功能：原候选查询仍返回登记时的区块标识与顺序。
	assertOriginalCandidate(t, sc)
	assertRoundKeepsExactlyTwoCandidates(t, sc)

	// 同一轮尚未决出：把编辑后的列表再次用于登记，按既有原因拒绝，
	// 原候选内容与票数（此时为 0）保持原样。
	if _, err := sc.n.RegisterCandidate(1, sc.list); reason(err) != ReasonUnknownTx {
		t.Fatalf("re-register [unknown a1 a1] got %v, want %s", err, ReasonUnknownTx)
	}
	assertOriginalCandidate(t, sc)
	assertRoundKeepsExactlyTwoCandidates(t, sc)

	// 名单内验证者向“原区块标识”投下第一张票：计入但尚不足以确认（需 3 票），
	// 这张票必须保留，候选顺序仍是登记时的 [a1, b1]。
	vres, err := sc.n.Vote(sc.keys[1].pub, 1, sc.altID)
	if err != nil || !vres.Counted || vres.Confirmed || vres.Block != nil {
		t.Fatalf("first vote after invalid list edit: %+v %v", vres, err)
	}
	assertOriginalCandidate(t, sc, sc.keys[1].pub)

	// 只含未知标识的列表再次登记仍以 unknown-transaction 拒绝，已有一张票保留。
	sc.list = []string{unknownID, b1ID}
	if _, err := sc.n.RegisterCandidate(1, sc.list); reason(err) != ReasonUnknownTx {
		t.Fatalf("re-register [unknown b1] got %v, want %s", err, ReasonUnknownTx)
	}
	assertOriginalCandidate(t, sc, sc.keys[1].pub)
	// 未知标识不会因编辑或失败登记成为节点中的交易，候选集合也不增不减。
	if _, err := sc.n.Tx(unknownID); reason(err) != ReasonUnknownTx {
		t.Fatalf("unknown id after rejected register got %v, want %s", err, ReasonUnknownTx)
	}
	assertRoundKeepsExactlyTwoCandidates(t, sc)

	// 第二张票继续投给原区块标识：仍计入但未确认，两张票都保留。
	vres, err = sc.n.Vote(sc.keys[2].pub, 1, sc.altID)
	if err != nil || !vres.Counted || vres.Confirmed {
		t.Fatalf("second vote must count without confirming: %+v %v", vres, err)
	}
	assertOriginalCandidate(t, sc, sc.keys[1].pub, sc.keys[2].pub)
	if sc.n.CurrentRound() != 1 || sc.n.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0 after two votes", sc.n.CurrentRound(), sc.n.Height())
	}

	// 只有有效标识但出现两次：以 duplicate-in-list 拒绝（不超上限、均在池中），
	// 原候选内容与已有两张票保持原样。
	sc.list = []string{a1ID, b1ID, a1ID}
	if _, err := sc.n.RegisterCandidate(1, sc.list); reason(err) != ReasonDuplicateInList {
		t.Fatalf("re-register [a1 b1 a1] got %v, want %s", err, ReasonDuplicateInList)
	}
	assertOriginalCandidate(t, sc, sc.keys[1].pub, sc.keys[2].pub)
	assertRoundKeepsExactlyTwoCandidates(t, sc)

	// 恢复调用方最初的列表后再次登记：相同列表只返回同一候选并保留已有票数。
	sc.list = []string{a1ID, b1ID}
	rres, err := sc.n.RegisterCandidate(1, sc.list)
	if err != nil {
		t.Fatalf("re-register original list: %v", err)
	}
	if !rres.Existing || rres.BlockID != sc.altID {
		t.Fatalf("re-register original = %+v, want existing %s", rres, sc.altID)
	}
	assertOriginalCandidate(t, sc, sc.keys[1].pub, sc.keys[2].pub)

	// 第三票达到严格超过三分之二门槛：立即确认，返回块采用原始列表与顺序。
	cres, err := sc.n.Vote(sc.keys[3].pub, 1, sc.altID)
	if err != nil || !cres.Counted || !cres.Confirmed || cres.Block == nil {
		t.Fatalf("third vote should confirm original candidate: %+v %v", cres, err)
	}
	if cres.Block.Height != 1 || cres.Block.Round != 1 ||
		cres.Block.ID != sc.altID || cres.Block.PreviousID != "" ||
		fmt.Sprint(cres.Block.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("confirming block must use the registered order [a1 b1]: %+v", cres.Block)
	}

	// 进入下一轮、确认高度推进到 1。
	if sc.n.CurrentRound() != 2 || sc.n.Height() != 1 {
		t.Fatalf("after confirm round=%d height=%d, want 2/1", sc.n.CurrentRound(), sc.n.Height())
	}

	// 按高度查询到的确认历史与最新块都采用原始交易列表与顺序。
	blk, err := sc.n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != sc.altID || blk.Height != 1 || blk.Round != 1 || blk.PreviousID != "" ||
		fmt.Sprint(blk.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("confirmed history must keep registered order: %+v", blk)
	}
	if latest, ok := sc.n.LatestBlock(); !ok ||
		latest.ID != sc.altID || fmt.Sprint(latest.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("latest block wrong: %+v ok=%v, want %s %v", latest, ok, sc.altID, wantOrder)
	}

	// 交易查询关联这个胜出块；被调用方后来写进列表的 a2 仍排队、未得到确认。
	for _, id := range wantOrder {
		info := sc.n.mustTx(t, id)
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != sc.altID {
			t.Fatalf("winning tx %s must be confirmed in block %s: %+v", id, sc.altID, info)
		}
	}
	info2 := sc.n.mustTx(t, a2ID)
	if info2.Status != StatusQueued || info2.BlockHeight != 0 || info2.BlockID != "" {
		t.Fatalf("edited-in a2 must stay queued and unconfirmed: %+v", info2)
	}

	// 各账户只推进到原候选实际包含的最后一个序号：A、B 均为 1。
	// A 账户下仅剩未被任何确认块包含的 a2 等待打包；B 账户已无待处理交易。
	assertAccountQueue(t, sc.n, sc.keys[0].pub, 1, map[string]string{
		a2ID: "waiting-pack",
	})
	assertAccountQueue(t, sc.n, sc.keys[1].pub, 1, map[string]string{})

	// 旧轮记录：原竞争候选胜出且保留登记时顺序与真实投票者，本地提议落选。
	// keys[0] 本轮从未投票，确认后仍按完整名单列在未投票名单中。
	rc, err := sc.n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.UnconfirmedEnd {
		t.Fatalf("decided round view wrong: %+v", rc)
	}
	if fmt.Sprint(pubHexList(rc.Unvoted)) != fmt.Sprint(sortedPubHex(sc.keys[0].pub)) {
		t.Fatalf("unvoted after confirm = %x, want only keys[0]", rc.Unvoted)
	}
	won := findCandidate(t, sc.n, 1, sc.altID)
	if won.Result != CandidateWon || won.Local ||
		fmt.Sprint(won.TxIDs) != fmt.Sprint(wantOrder) ||
		fmt.Sprint(pubHexList(won.Voters)) != fmt.Sprint(sortedPubHex(sc.keys[1].pub, sc.keys[2].pub, sc.keys[3].pub)) {
		t.Fatalf("won candidate view wrong: %+v", won)
	}
	lost := findCandidate(t, sc.n, 1, sc.local.BlockID)
	if lost.Result != CandidateLost || !lost.Local ||
		fmt.Sprint(lost.TxIDs) != fmt.Sprint([]string{b1ID, a1ID}) || len(lost.Voters) != 0 {
		t.Fatalf("lost local proposal view wrong: %+v", lost)
	}
}

// TestRegisterCandidateDetachesInternalTxIDsFromCallerList 直接在同包内钉住
// 入口的脱离动作：登记成功后，内部候选保存的交易标识切片与调用方传入的切片
// 不共享底层数组；调用方随后原地改写原切片，内部候选的列表逐字保持登记时内容。
func TestRegisterCandidateDetachesInternalTxIDsFromCallerList(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 5, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 9, 100)
	if _, err := n.Submit(a1); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(b1); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Propose(); err != nil {
		t.Fatal(err)
	}

	caller := []string{a1.ID(), b1.ID()}
	res, err := n.RegisterCandidate(1, caller)
	if err != nil {
		t.Fatal(err)
	}
	internal := n.st.Rounds[1].Candidates[res.BlockID]
	if internal == nil || len(internal.TxIDs) != 2 {
		t.Fatalf("internal candidate missing or wrong: %+v", internal)
	}
	// 入口必须复制切片：两个切片的首元素不能落在同一底层数组。
	if &internal.TxIDs[0] == &caller[0] {
		t.Fatal("internal candidate TxIDs shares backing array with caller list")
	}

	// 调用方尽力改写原切片：交换、改成未知标识、追加长度。
	caller[0], caller[1] = caller[1], caller[0]
	caller[0] = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	caller = append(caller, a1.ID())
	if internal.TxIDs[0] != a1.ID() || internal.TxIDs[1] != b1.ID() {
		t.Fatalf("internal candidate changed with caller slice: %v, want [a1 b1]", internal.TxIDs)
	}
	if len(internal.TxIDs) != 2 {
		t.Fatalf("internal candidate length = %d, want 2", len(internal.TxIDs))
	}
}
