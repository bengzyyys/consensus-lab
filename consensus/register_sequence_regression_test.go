package consensus

import (
	"fmt"
	"testing"
)

// 本文件钉住“竞争候选登记的账户序号规则”回归保障，只补充测试、不改变公开入口
// 与现有交易规则。规则有两个易错点：
//
//  1. 起点判断必须以各账户“已确认序号加一”为准。交易可以先在池中等待，候选
//     却不能借用没有写进自己列表的前序交易——前序交易即便仍在池中、甚至已被
//     本地提议或其他候选引用，本次候选从更后序号开始或中途跳过它都必须以
//     sequence-not-consecutive 拒绝。
//  2. 连续性必须按交易“在调用者列表中的实际出现次序”逐账户判断。不同账户可以
//     交错出现；某账户自己的序号集合连续、但后一个序号先于前一个出现（其他账户
//     夹在中间）同样以 sequence-not-consecutive 拒绝。
//
// 场景（四验证者，单块上限 4）：第 1 轮确认 A1、B1，制造确认历史——账户 A、B
// 已确认序号为 1，账户 C 仍为 0，下一高度为 2、前块为第 1 轮确认块。第 2 轮
// 池中放入 A2、A3、A4、B2、C1、C2，费用刻意与序号脱钩；本地提议按既有打包
// 规则冻结为 [B2, C1, C2, A2]，A3、A4 留在池中排队。再登记一个持有两张票的
// 既有竞争候选 [B2, A2]，用于检查成功登记与各类拒绝都不动原有候选票数。
//
// 合法新候选列表 [A2, B2, A3, C1]：相邻交易来自不同账户（A、B、A、C 交错），
// 各账户都从自己的已确认序号加一（A/B 从 2、C 从 1）起按出现次序连续，费用
// 1、20、8、2 既不按本地打包顺序也不按费用排序。登记必须成功，按轮次查询必须
// 原样保留调用者给出的完整交易顺序（不重新按账户、序号或费用排列）；本地提议
// 标识与顺序冻结不变；登记不推进账户已确认序号、不产生确认块、轮次不前进；
// 新候选实际引用的排队交易 A3 才转为等待投票，已被本地提议/既有候选引用的
// A2、B2、C1、C2 继续等待投票，始终未被引用的 A4 继续等待打包。
//
// 拒绝列表均满足其他登记条件（不超过单块上限 4、无重复、标识已知且仍在池中），
// 因此只能因序号被拒：
//   - [A3, B2, C1]：A 从序号 3 开始，跳过的 A2 不仅在池中还被本地提议与既有
//     候选引用；
//   - [A2, B2, A4, C1]：A 中途跳过仍在池中的 A3；
//   - [A3, B2, C1, A2]：A 的 2、3 都在列表中，但 3 先于 2 出现，B、C 夹在
//     中间不能掩盖倒序；
//   - [C2, A2, B2, C1]：C 的 2 先于 1 出现，中间隔着 A、B 的交易。
//
// 拒绝在成功登记前后各执行一遍：成功前 A3 仍只在排队，失败登记不能把它锁成
// 等待投票；成功后 A3 已等待投票，失败登记也不能把它解锁。任何拒绝后候选集合、
// 本地提议、既有候选票数、未投票名单、各交易状态与账户已确认序号一律保持原样。

// seqScene 收拢序号回归场景的关键句柄。
type seqScene struct {
	// local1 是第 1 轮已确认的本地提议（确认历史，前序块）。
	local1 ProposalView
	// local2 是第 2 轮冻结的本地提议：[b2, c1, c2, a2]。
	local2 ProposalView
	// alt1ID 是持有两张票的既有竞争候选 [b2, a2]。
	alt1ID string
	// 第 2 轮池中交易：a2/a3/a4 属于账户 A（已确认序号 1），
	// b2 属于账户 B（已确认序号 1），c1/c2 属于账户 C（已确认序号 0）。
	a2, a3, a4, b2, c1, c2 *Transaction
	// alt2List 是本轮受测的合法交错列表 [a2, b2, a3, c1]。
	alt2List []string
}

// buildSeqScene 在单块上限 4 的节点上构造第 2 轮未决场景，并返回关键句柄。
func buildSeqScene(t *testing.T, n *Node, keys []testKey) seqScene {
	t.Helper()

	// 第 1 轮：确认 A1(费7)、B1(费3)，为 A、B 制造已确认序号 1 的历史。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 7, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 3, 100)
	for _, tx := range []*Transaction{a1, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}
	local1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local1.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("setup: round 1 proposal = %v, want [a1 b1]", local1.TxIDs)
	}
	confirmByVotes(t, n, keys, local1.BlockID)
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("setup: round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}

	// 第 2 轮：费用与序号脱钩，便于证明候选顺序不按费用/账号重排。
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 1, 100)
	a3 := NewTransaction(keys[0].priv, 3, []byte("a3"), 8, 100)
	a4 := NewTransaction(keys[0].priv, 4, []byte("a4"), 5, 100)
	b2 := NewTransaction(keys[1].priv, 2, []byte("b2"), 20, 100)
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), 2, 100)
	c2 := NewTransaction(keys[2].priv, 2, []byte("c2"), 30, 100)
	for _, tx := range []*Transaction{a2, a3, a4, b2, c1, c2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}
	// 上限 4 的既有打包规则：B2(20) → C1(2) → C2(30) → A2(1)；
	// C 账户在选入 C1 后立即可以考虑 C2，故 C2(30) 在 A2(1) 之前被选入；
	// A3 被 A2 的缺口挡住、A4 又被 A3 挡住，二者留在池中排队。
	local2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local2.TxIDs) != fmt.Sprint([]string{b2.ID(), c1.ID(), c2.ID(), a2.ID()}) {
		t.Fatalf("setup: round 2 proposal = %v, want [b2 c1 c2 a2]", local2.TxIDs)
	}

	// 既有竞争候选 [b2, a2]，与本地提议顺序、集合都不同，随后持有两张票。
	alt1, err := n.RegisterCandidate(2, []string{b2.ID(), a2.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt1.Existing || alt1.BlockID == local2.BlockID {
		t.Fatalf("setup: alt1 must be a fresh candidate: %+v local=%s", alt1, local2.BlockID)
	}
	for _, i := range []int{1, 2} {
		res, err := n.Vote(keys[i].pub, 2, alt1.BlockID)
		if err != nil || !res.Counted || res.Confirmed {
			t.Fatalf("setup vote for alt1 by validator %d: %+v %v", i, res, err)
		}
	}

	return seqScene{
		local1:   local1,
		local2:   local2,
		alt1ID:   alt1.BlockID,
		a2:       a2,
		a3:       a3,
		a4:       a4,
		b2:       b2,
		c1:       c1,
		c2:       c2,
		alt2List: []string{a2.ID(), b2.ID(), a3.ID(), c1.ID()},
	}
}

// seqBadLists 返回各序号拒绝用例：名称 -> 调用者给出的交易列表。
func (sc seqScene) badLists() map[string][]string {
	return map[string][]string{
		"start-after-missing-predecessor": {sc.a3.ID(), sc.b2.ID(), sc.c1.ID()},
		"skip-predecessor-mid-list":       {sc.a2.ID(), sc.b2.ID(), sc.a4.ID(), sc.c1.ID()},
		"reverse-order-with-others-between": {
			sc.a3.ID(), sc.b2.ID(), sc.c1.ID(), sc.a2.ID(),
		},
		"reverse-order-across-other-accounts": {
			sc.c2.ID(), sc.a2.ID(), sc.b2.ID(), sc.c1.ID(),
		},
	}
}

// assertSeqRoundState 断言第 2 轮未决状态的不变部分：轮次 2、高度 1、第 1 轮
// 确认块不变；本地提议标识与交易顺序冻结；候选按区块标识排序返回，本地候选
// 0 票、alt1 保留 keys[1]/keys[2] 两张票，未投票名单为 keys[0]/keys[3]；
// 账户已确认序号 A=1、B=1、C=0。alt2ID 为空时恰有两个候选，否则还必须包含
// 以调用者原始顺序登记的 alt2（非本地、0 票、pending）。
func assertSeqRoundState(t *testing.T, n *Node, sc seqScene, keys []testKey, alt2ID string) {
	t.Helper()
	if n.CurrentRound() != 2 {
		t.Fatalf("current round = %d, want 2", n.CurrentRound())
	}
	if n.Height() != 1 {
		t.Fatalf("confirmed height = %d, want 1", n.Height())
	}
	blk, err := n.BlockAt(1)
	if err != nil || blk.ID != sc.local1.BlockID {
		t.Fatalf("confirmed block changed: %+v %v, want %s", blk, err, sc.local1.BlockID)
	}

	p, ok := n.Proposal()
	if !ok || p.BlockID != sc.local2.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(sc.local2.TxIDs) {
		t.Fatalf("local proposal must stay frozen: %+v ok=%v, want id=%s order=%v",
			p, ok, sc.local2.BlockID, sc.local2.TxIDs)
	}

	rc, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 2 must stay pending with records: %+v", rc)
	}
	wantCount := 2
	if alt2ID != "" {
		wantCount = 3
	}
	if len(rc.Candidates) != wantCount {
		t.Fatalf("candidate count = %d, want %d", len(rc.Candidates), wantCount)
	}
	wantUnvoted := [][]byte{keys[0].pub, keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
	}

	var gotLocal, gotAlt1, gotAlt2 *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case sc.local2.BlockID:
			gotLocal = &rc.Candidates[i]
		case sc.alt1ID:
			gotAlt1 = &rc.Candidates[i]
		case alt2ID:
			gotAlt2 = &rc.Candidates[i]
		}
	}
	if gotLocal == nil || gotAlt1 == nil {
		t.Fatalf("local and alt1 candidates must remain: %+v", rc.Candidates)
	}
	if !gotLocal.Local || gotLocal.Result != CandidatePending || len(gotLocal.Voters) != 0 ||
		fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint(sc.local2.TxIDs) {
		t.Fatalf("local candidate wrong: %+v", gotLocal)
	}
	wantAlt1Order := []string{sc.b2.ID(), sc.a2.ID()}
	if gotAlt1.Local || gotAlt1.Result != CandidatePending ||
		fmt.Sprint(gotAlt1.TxIDs) != fmt.Sprint(wantAlt1Order) {
		t.Fatalf("alt1 candidate wrong: %+v", gotAlt1)
	}
	wantAlt1Voters := [][]byte{keys[1].pub, keys[2].pub}
	if fmt.Sprint(hexKeys(gotAlt1.Voters)) != fmt.Sprint(hexKeys(wantAlt1Voters)) {
		t.Fatalf("alt1 votes = %x, want %x", gotAlt1.Voters, wantAlt1Voters)
	}
	if alt2ID != "" {
		if gotAlt2 == nil {
			t.Fatalf("new candidate %s missing from round record", alt2ID)
		}
		if gotAlt2.Local || gotAlt2.Result != CandidatePending || len(gotAlt2.Voters) != 0 {
			t.Fatalf("alt2 must be a non-local pending candidate with no votes: %+v", gotAlt2)
		}
		// 查询必须保留调用者给出的完整顺序，不按账户、序号或费用重排。
		if fmt.Sprint(gotAlt2.TxIDs) != fmt.Sprint(sc.alt2List) {
			t.Fatalf("alt2 tx order = %v, want caller order %v", gotAlt2.TxIDs, sc.alt2List)
		}
	}

	// 登记（无论成功或被拒）都不推进账户已确认序号。
	if a := n.Account(keys[0].pub); a.ConfirmedSequence != 1 {
		t.Fatalf("account A confirmed sequence = %d, want 1", a.ConfirmedSequence)
	}
	if a := n.Account(keys[1].pub); a.ConfirmedSequence != 1 {
		t.Fatalf("account B confirmed sequence = %d, want 1", a.ConfirmedSequence)
	}
	if a := n.Account(keys[2].pub); a.ConfirmedSequence != 0 {
		t.Fatalf("account C confirmed sequence = %d, want 0", a.ConfirmedSequence)
	}
}

// assertSeqStatusesBeforeAlt2 断言成功登记 alt2 之前的交易等待状态：
// 只有本地提议/alt1 引用的交易等待投票；A3、A4 仅在池中排队等待打包。
func assertSeqStatusesBeforeAlt2(t *testing.T, n *Node, sc seqScene, keys []testKey) {
	t.Helper()
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{
		sc.a2.ID(): "waiting-vote", // 本地提议 + alt1
		sc.a3.ID(): "waiting-pack", // 仅排队：失败登记不能锁定它
		sc.a4.ID(): "waiting-pack", // 仅排队
	})
	assertAccountQueue(t, n, keys[1].pub, 1, map[string]string{
		sc.b2.ID(): "waiting-vote",
	})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{
		sc.c1.ID(): "waiting-vote",
		sc.c2.ID(): "waiting-vote",
	})
}

// assertSeqStatusesAfterAlt2 断言成功登记 alt2 之后的交易等待状态：
// 新候选实际引用的排队交易 A3 转为等待投票；A4 仍只在排队，其余不变。
func assertSeqStatusesAfterAlt2(t *testing.T, n *Node, sc seqScene, keys []testKey) {
	t.Helper()
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{
		sc.a2.ID(): "waiting-vote",
		sc.a3.ID(): "waiting-vote", // alt2 首次引用后才转为等待投票
		sc.a4.ID(): "waiting-pack", // 未被任何候选引用，继续等待打包
	})
	assertAccountQueue(t, n, keys[1].pub, 1, map[string]string{
		sc.b2.ID(): "waiting-vote",
	})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{
		sc.c1.ID(): "waiting-vote",
		sc.c2.ID(): "waiting-vote",
	})
}

// rejectSeqLists 对全部序号拒绝用例执行登记，要求一律以 sequence-not-consecutive
// 拒绝，并保证每个列表本身满足其他登记条件（4 笔以内、无重复、标识已知且在池中）。
func rejectSeqLists(t *testing.T, n *Node, sc seqScene) {
	t.Helper()
	bad := sc.badLists()
	// 固定顺序：起点跳过 → 中途跳过 → 倒序（A）→ 倒序（C），避免 map 遍历序
	// 影响回归输出。
	ordered := []string{
		"start-after-missing-predecessor",
		"skip-predecessor-mid-list",
		"reverse-order-with-others-between",
		"reverse-order-across-other-accounts",
	}
	for _, name := range ordered {
		list, ok := bad[name]
		if !ok {
			t.Fatalf("missing bad-list case %q", name)
		}
		if uint64(len(list)) > 4 {
			t.Fatalf("case %s: list length %d exceeds the block limit 4; "+
				"too-many-transactions must not mask the sequence rejection", name, len(list))
		}
		failedID := BlockID(2, 2, sc.local1.BlockID, list)
		if _, err := n.RegisterCandidate(2, list); reason(err) != ReasonSequenceGap {
			t.Fatalf("case %s: got %v, want %s", name, err, ReasonSequenceGap)
		}
		// 被拒候选的区块标识绝不能进入候选集合。
		rc, err := n.Candidates(2)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range rc.Candidates {
			if c.BlockID == failedID {
				t.Fatalf("case %s: rejected candidate %s must not be registered", name, failedID)
			}
		}
	}
}

// TestRegisterCandidateInterleavedSequence 钉住合法交错候选的登记行为：
// 有确认历史时起点按各账户已确认序号加一，账户间交错、费用乱序也必须成功；
// 查询保留调用者完整顺序；只有新候选实际引用的排队交易才转为等待投票；
// 本地提议、账户序号、确认块与轮次均不变，且结果可持久化恢复。
func TestRegisterCandidateInterleavedSequence(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 4)
	sc := buildSeqScene(t, n, keys)

	// 成功前：A3、A4 等待打包，其余已引用交易等待投票，候选只有本地与 alt1。
	assertSeqStatusesBeforeAlt2(t, n, sc, keys)
	assertSeqRoundState(t, n, sc, keys, "")

	wantID := BlockID(2, 2, sc.local1.BlockID, sc.alt2List)
	if wantID == sc.local2.BlockID || wantID == sc.alt1ID {
		t.Fatal("setup: interleaved candidate must differ from local and alt1")
	}
	res, err := n.RegisterCandidate(2, append([]string{}, sc.alt2List...))
	if err != nil {
		t.Fatalf("interleaved consecutive candidate must register: %v", err)
	}
	if res.Existing || res.BlockID != wantID {
		t.Fatalf("register result = %+v, want fresh candidate %s", res, wantID)
	}

	// 成功后：调用者顺序原样保留；只有 A3 新转等待投票；A4 继续等待打包。
	assertSeqRoundState(t, n, sc, keys, wantID)
	assertSeqStatusesAfterAlt2(t, n, sc, keys)

	// 第 1 轮确认的交易仍是确认状态，登记没有产生第二个确认块。
	for _, id := range []string{sc.local1.TxIDs[0], sc.local1.TxIDs[1]} {
		info := n.mustTx(t, id)
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != sc.local1.BlockID {
			t.Fatalf("confirmed tx %s wrong after register: %+v", id, info)
		}
	}

	// 相同列表重复登记返回同一候选、保留票数语义（此处 0 票），状态不再变化。
	res2, err := n.RegisterCandidate(2, append([]string{}, sc.alt2List...))
	if err != nil || !res2.Existing || res2.BlockID != wantID {
		t.Fatalf("duplicate register = %+v, %v, want existing %s", res2, err, wantID)
	}
	assertSeqRoundState(t, n, sc, keys, wantID)
	assertSeqStatusesAfterAlt2(t, n, sc, keys)

	// 持久化：重开后候选顺序、票数、未投票名单与两类等待状态完全一致。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertSeqRoundState(t, reopened, sc, keys, wantID)
	assertSeqStatusesAfterAlt2(t, reopened, sc, keys)
}

// TestRegisterCandidateSequenceRejections 钉住两类易误判的序号拒绝：
// 前序交易仍在池中（甚至已被本地提议引用）不能被候选借用，以及账户内倒序
// 不能被其他账户的交错排列掩盖。拒绝在成功登记 alt2 前后各执行一遍：
// 成功前不得锁定只在排队的 A3/A4，成功后不得解锁已等待投票的 A3；
// 候选集合、本地提议、原有候选票数与账户序号在任何拒绝后保持原样。
func TestRegisterCandidateSequenceRejections(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 4)
	sc := buildSeqScene(t, n, keys)

	// 成功登记之前：A3、A4 只在排队。任何序号拒绝都不得把它们锁成等待投票，
	// 也不得改变候选集合、本地提议或 alt1 的两张票。
	rejectSeqLists(t, n, sc)
	assertSeqRoundState(t, n, sc, keys, "")
	assertSeqStatusesBeforeAlt2(t, n, sc, keys)

	// 合法登记 alt2：A3 转为等待投票，A4 仍排队。
	alt2, err := n.RegisterCandidate(2, append([]string{}, sc.alt2List...))
	if err != nil {
		t.Fatalf("valid interleaved candidate must register before the second rejection pass: %v", err)
	}
	assertSeqRoundState(t, n, sc, keys, alt2.BlockID)
	assertSeqStatusesAfterAlt2(t, n, sc, keys)

	// 成功登记之后再次执行同一组拒绝：A3 已等待投票，失败登记不得解锁它，
	// A4 仍须等待打包；三候选与票数分布一律不变。
	rejectSeqLists(t, n, sc)
	assertSeqRoundState(t, n, sc, keys, alt2.BlockID)
	assertSeqStatusesAfterAlt2(t, n, sc, keys)

	// 被 alt2 引用后，A3 的同序号加费替换必须继续按现有锁定原因拒绝；
	// 失败登记绝不能解除这层保护。
	a3hi := NewTransaction(keys[0].priv, 3, []byte("a3-hi"), 90, 100)
	if _, err := n.Submit(a3hi); reason(err) != ReasonProposalLocked {
		t.Fatalf("a3 replacement after failed registers got %v, want %s", err, ReasonProposalLocked)
	}
	if info := n.mustTx(t, sc.a3.ID()); info.Status != StatusProposed || info.ReplacedBy != "" {
		t.Fatalf("a3 must stay proposed without replacement link: %+v", info)
	}
	// 始终未被引用的 A4 仍可按原规则加费替换，证明它没被失败登记锁定。
	a4hi := NewTransaction(keys[0].priv, 4, []byte("a4-hi"), 90, 100)
	rep, err := n.Submit(a4hi)
	if err != nil {
		t.Fatalf("queued-only a4 must remain replaceable after failed registers: %v", err)
	}
	if rep.ReplacedID != sc.a4.ID() {
		t.Fatalf("a4 replacement = %+v, want ReplacedID %s", rep, sc.a4.ID())
	}

	// 落盘恢复后仍是三候选、同样票数与锁定/排队状态。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertSeqRoundState(t, reopened, sc, keys, alt2.BlockID)
}
