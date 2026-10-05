package consensus

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// returnedEvictionScene 固定“竞争候选确认后，落选候选独有交易回到排队，
// 旧轮次候选只作历史保留”的场景：
//
//	keys[0] ret    费用 20，序号 1，到期 100：仅本地提议引用；竞争候选只
//	               包含共享交易而胜出后，它作为落选候选独有且未到期的交易
//	               回到排队，是后续排队交易中唯一费用最低的可淘汰对象
//	keys[0] ret2   费用 60，序号 2：同账户后续交易，提议时因序号缺口未入选，
//	               始终排队；ret 被挤出后留下最早缺口 1
//	keys[1] shared 费用 30：本地提议与竞争候选共享，随胜出块确认
//	keys[2] back   费用 50，到期 100：提议冻结后提交，始终排队
//	keys[3]：验证者，始终未投票
//	keys[4]：新轮次填补空位与挤出提交使用的发送者
//
// 容量 4、单块上限 2、四名验证者。第 1 轮池恰好满（四笔占位）：本地提议
// [shared ret] 冻结后提交 back；竞争候选只登记 [shared]，取得三票确认。
// 进入第 2 轮后 shared 确认释放一个位置，ret 回到排队与 ret2、back 共占
// 三个位置。旧轮次仍保留两个候选及其交易顺序、投票者与 won/lost 结果。
type returnedEvictionScene struct {
	n      *Node
	keys   []testKey
	ret    *Transaction
	ret2   *Transaction
	shared *Transaction
	back   *Transaction
	local  ProposalView
	altID  string
}

func newReturnedEvictionScene(t *testing.T) *returnedEvictionScene {
	t.Helper()
	keys := genKeys(t, 5)
	vals := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		vals[i] = keys[i].pub
	}
	n, err := New(t.TempDir(), Config{
		Seed: []byte("lost-candidate-return-scene"), Validators: vals,
		MaxTxsPerBlock: 2, PoolCapacity: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &returnedEvictionScene{n: n, keys: keys}
	// 先提交进入本地提议的两笔及同账户的后续序号交易：费用高的 shared 在
	// 提议首位，ret 账户的序号 2 交易 ret2 因序号缺口本轮不能入选。
	s.shared = NewTransaction(keys[1].priv, 1, []byte("shared"), 30, 100)
	s.ret = NewTransaction(keys[0].priv, 1, []byte("local-only-returned"), 20, 100)
	s.ret2 = NewTransaction(keys[0].priv, 2, []byte("local-seq2"), 60, 100)
	for _, tx := range []*Transaction{s.shared, s.ret, s.ret2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(local.TxIDs, []string{s.shared.ID(), s.ret.ID()}) {
		t.Fatalf("local proposal = %v, want [shared ret]", local.TxIDs)
	}
	s.local = local

	// 提议冻结后提交始终排队的 back，池恰好满（4 笔）。
	s.back = NewTransaction(keys[2].priv, 1, []byte("always-queued"), 50, 100)
	if _, err := n.Submit(s.back); err != nil {
		t.Fatal(err)
	}
	if got := n.st.poolSize(); got != 4 {
		t.Fatalf("pool size = %d, want 4 (full) before voting", got)
	}

	// 竞争候选只包含本地提议中的部分交易（共享交易）。
	s.altID = registerMust(t, n, 1, []string{s.shared.ID()})
	if s.altID == s.local.BlockID {
		t.Fatal("competing candidate must have a distinct block id")
	}

	// 四人名单需严格超过三分之二，即三票；前三张票投给竞争候选并立即确认。
	for i := 0; i < 2; i++ {
		res, err := n.Vote(keys[i].pub, 1, s.altID)
		if err != nil || !res.Counted || res.Confirmed {
			t.Fatalf("vote %d must count without confirming: %+v %v", i+1, res, err)
		}
	}
	res, err := n.Vote(keys[2].pub, 1, s.altID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("third vote should confirm competing candidate: %+v %v", res, err)
	}
	if res.Block.Height != 1 || res.Block.Round != 1 || res.Block.ID != s.altID ||
		res.Block.PreviousID != "" || !reflect.DeepEqual(res.Block.TxIDs, []string{s.shared.ID()}) {
		t.Fatalf("confirming block wrong: %+v", res.Block)
	}
	return s
}

// assertPostConfirmShape 固定确认刚完成时的形态：
//   - 进入第 2 轮，高度 1；共享交易关联胜出块，其账户序号推进；
//   - 落选候选独有且未到期的 ret 回到排队，内容原样，账户已确认序号保持 0；
//   - 池占用 3（确认释放了一个位置）；
//   - 旧轮次两个候选的交易顺序、投票者与 won/lost 结果保持原样。
func (s *returnedEvictionScene) assertPostConfirmShape(t *testing.T) {
	t.Helper()
	n := s.n
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1 after confirm", n.CurrentRound(), n.Height())
	}
	if got := n.st.poolSize(); got != 3 {
		t.Fatalf("pool size = %d, want 3 after shared confirmed", got)
	}
	// 新轮次尚未产生本地提议，也就没有任何未决候选再次引用回池交易。
	if _, ok := n.Proposal(); ok {
		t.Fatal("round 2 must not have a local proposal yet")
	}

	// 共享交易随胜出块确认一次。
	info, err := n.Tx(s.shared.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusConfirmed || info.BlockID != s.altID || info.BlockHeight != 1 {
		t.Fatalf("shared tx = %+v, want confirmed in winning block", info)
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != s.altID || blk.PreviousID != "" ||
		!reflect.DeepEqual(blk.TxIDs, []string{s.shared.ID()}) {
		t.Fatalf("stored winning block wrong: %+v", blk)
	}
	if latest, ok := n.LatestBlock(); !ok || latest.ID != s.altID {
		t.Fatalf("latest block = %+v ok=%v, want winning block", latest, ok)
	}
	assertAccountQueue(t, n, s.keys[1].pub, 1, map[string]string{})

	// 落选候选独有交易 ret：回到排队、未到期、完整内容保留，序号不推进；
	// 同账户 ret2 与 back 始终排队且内容原样。
	for _, tx := range []*Transaction{s.ret, s.ret2, s.back} {
		info := mustTx(t, n, tx.ID())
		if info.Status != StatusQueued || info.Note != "" ||
			info.BlockID != "" || info.BlockHeight != 0 ||
			info.DropReason != "" || info.ReplacedBy != "" {
			t.Fatalf("returned/queued tx %s = %+v, want plain queued without links", tx.ID(), info)
		}
		assertTxMatches(t, info.Tx, tx)
	}
	assertAccountQueue(t, n, s.keys[0].pub, 0, map[string]string{
		s.ret.ID(): "waiting-pack", s.ret2.ID(): "waiting-pack",
	})
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.back.ID(): "waiting-pack"})

	// 回池交易立刻可作为容量淘汰对象：当前没有任何未决候选引用它，
	// 是否受保护只取决于当前引用，而不是它曾经进入过候选。
	victim, ok := n.st.evictionVictim()
	if !ok || victim != s.ret.ID() {
		t.Fatalf("eviction victim = %s ok=%v, want returned ret %s", victim, ok, s.ret.ID())
	}

	// 旧轮次历史：竞争候选胜出、本地提议落选，交易顺序、投票者原样冻结。
	rc := mustCandidates(t, n, 1)
	if !rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords || len(rc.Candidates) != 2 {
		t.Fatalf("round 1 record wrong: %+v", rc)
	}
	alt := candidateByBlockID(rc, s.altID)
	local := candidateByBlockID(rc, s.local.BlockID)
	if alt == nil || local == nil {
		t.Fatalf("both round 1 candidates must remain in history: %+v", rc.Candidates)
	}
	if !reflect.DeepEqual(alt.TxIDs, []string{s.shared.ID()}) ||
		alt.Result != CandidateWon || alt.Local {
		t.Fatalf("winning candidate history wrong: %+v", alt)
	}
	if !reflect.DeepEqual(local.TxIDs, []string{s.shared.ID(), s.ret.ID()}) ||
		local.Result != CandidateLost || !local.Local || len(local.Voters) != 0 {
		t.Fatalf("losing local candidate history wrong: %+v", local)
	}
	wantVoters := []string{
		fmt.Sprintf("%x", s.keys[0].pub),
		fmt.Sprintf("%x", s.keys[1].pub),
		fmt.Sprintf("%x", s.keys[2].pub),
	}
	sort.Strings(wantVoters)
	gotVoters := make([]string, len(alt.Voters))
	for i, v := range alt.Voters {
		gotVoters[i] = fmt.Sprintf("%x", v)
	}
	if !reflect.DeepEqual(gotVoters, wantVoters) {
		t.Fatalf("winner voters = %v, want %v", gotVoters, wantVoters)
	}
	wantUnvoted := []string{fmt.Sprintf("%x", s.keys[3].pub)}
	gotUnvoted := make([]string, len(rc.Unvoted))
	for i, v := range rc.Unvoted {
		gotUnvoted[i] = fmt.Sprintf("%x", v)
	}
	if !reflect.DeepEqual(gotUnvoted, wantUnvoted) {
		t.Fatalf("unvoted = %v, want %v", gotUnvoted, wantUnvoted)
	}
}

// 完整衔接过程：竞争候选确认后，落选候选独有且未到期的交易回到排队；
// 新轮次中旧轮次的候选引用只是历史，不能继续保护它。确认释放的位置先被
// 正常填补（无挤出标识）；池再次满员后，回池交易作为唯一最低费排队交易
// 被费用严格更高的新交易挤出，dropped/pool-capacity/新轮次，内容保留、
// 无确认块与替换关联，账户已确认序号不推进；旧轮候选顺序、投票记录与
// 胜出块关联保持原样。低费新交易仍被 pool-full 拒绝，不留历史、不改状态。
func TestLostCandidateReturnedTxRejoinsCapacityEviction(t *testing.T) {
	s := newReturnedEvictionScene(t)
	n := s.n
	s.assertPostConfirmShape(t)

	// 历史快照：后续提交不得改动旧轮次候选与胜出块。
	rcBefore := mustCandidates(t, n, 1)
	blockBefore, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}

	// 阶段一：确认释放了一个位置（3/4），用费用高于回池交易的新交易填补。
	fill := NewTransaction(s.keys[4].priv, 1, []byte("filler"), 41, 100)
	res, err := n.Submit(fill)
	if err != nil {
		t.Fatalf("free slot must accept new tx: %v", err)
	}
	if res.TxID != fill.ID() || res.EvictedID != "" || res.ReplacedID != "" {
		t.Fatalf("filler submit result = %+v, want accept without eviction/replace markers", res)
	}
	if got := n.st.poolSize(); got != 4 {
		t.Fatalf("pool size = %d, want 4 after filling freed slot", got)
	}
	// 回池交易与其余排队交易全部保留。
	for _, tx := range []*Transaction{s.ret, s.ret2, s.back} {
		if info, _ := n.Tx(tx.ID()); info.Status != StatusQueued {
			t.Fatalf("tx %s status = %s, want queued after free-slot fill", tx.ID(), info.Status)
		}
	}
	assertAccountQueue(t, n, s.keys[4].pub, 0, map[string]string{fill.ID(): "waiting-pack"})
	if rc := mustCandidates(t, n, 1); !reflect.DeepEqual(rc, rcBefore) {
		t.Fatalf("round 1 history changed after filler submit:\nbefore=%+v\nafter =%+v", rcBefore, rc)
	}

	// 阶段二：池再次满员。回池交易 ret（费用 20）是排队交易中费用最低的
	// 唯一对象；另一账户序号 2、费用严格更高的新交易必须挤出它。
	bump := NewTransaction(s.keys[4].priv, 2, []byte("bump"), 41, 100)
	res, err = n.Submit(bump)
	if err != nil {
		t.Fatalf("strictly higher fee must evict the returned tx: %v", err)
	}
	if res.TxID != bump.ID() || res.EvictedID != s.ret.ID() || res.ReplacedID != "" {
		t.Fatalf("bump submit result = %+v, want evicted ret with no replace link", res)
	}

	// 被挤出的回池交易：dropped / pool-capacity / 发生在新轮次 2，
	// 完整内容仍可查，不带确认块或替换关联。
	retInfo, err := n.Tx(s.ret.ID())
	if err != nil {
		t.Fatal(err)
	}
	if retInfo.Status != StatusDropped || retInfo.DropReason != DropReasonPoolCapacity || retInfo.DropRound != 2 {
		t.Fatalf("returned ret after eviction = %+v, want dropped/pool-capacity/round 2", retInfo)
	}
	if retInfo.BlockHeight != 0 || retInfo.BlockID != "" || retInfo.ReplacedBy != "" {
		t.Fatalf("dropped ret must not carry block/replace links: %+v", retInfo)
	}
	assertTxMatches(t, retInfo.Tx, s.ret)

	// 被挤出账户：待处理列表移除旧交易，已确认序号不推进；ret2 留下且
	// 账户查询指出最早缺口序号 1。
	acct0 := n.Account(s.keys[0].pub)
	if acct0.ConfirmedSequence != 0 || acct0.Gap != 1 || len(acct0.Pending) != 1 {
		t.Fatalf("ret sender account = %+v, want confirmed 0 gap 1 one pending (ret2)", acct0)
	}
	if acct0.Pending[0].ID != s.ret2.ID() ||
		acct0.Pending[0].Status != StatusQueued || acct0.Pending[0].Note != "waiting-pack" {
		t.Fatalf("remaining pending = %+v, want ret2 queued/waiting-pack", acct0.Pending[0])
	}
	assertTxMatches(t, acct0.Pending[0].Tx, s.ret2)

	// 新交易排队等待打包；新账户两笔交易按序号保留原有顺序与内容。
	if info, _ := n.Tx(bump.ID()); info.Status != StatusQueued {
		t.Fatalf("bump status = %s, want queued", info.Status)
	}
	acct4 := n.Account(s.keys[4].pub)
	if acct4.ConfirmedSequence != 0 || acct4.Gap != 0 || len(acct4.Pending) != 2 {
		t.Fatalf("bump sender account = %+v, want two pending without gap", acct4)
	}
	if acct4.Pending[0].ID != fill.ID() || acct4.Pending[1].ID != bump.ID() {
		t.Fatalf("new sender pending order = [%s %s], want [fill bump]", acct4.Pending[0].ID, acct4.Pending[1].ID)
	}
	for _, p := range acct4.Pending {
		if p.Status != StatusQueued || p.Note != "waiting-pack" {
			t.Fatalf("new sender pending %s = %+v, want queued/waiting-pack", p.ID, p)
		}
	}
	assertTxMatches(t, acct4.Pending[0].Tx, fill)
	assertTxMatches(t, acct4.Pending[1].Tx, bump)
	// 其他排队交易保留原有顺序和内容。
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.back.ID(): "waiting-pack"})
	assertTxMatches(t, mustTx(t, n, s.back.ID()).Tx, s.back)

	// 胜出交易的确认块关联与账户已确认序号不因淘汰变化。
	if info, _ := n.Tx(s.shared.ID()); info.Status != StatusConfirmed ||
		info.BlockID != s.altID || info.BlockHeight != 1 {
		t.Fatalf("winner link changed after eviction: %+v", info)
	}
	assertAccountQueue(t, n, s.keys[1].pub, 1, map[string]string{})
	if got := n.st.poolSize(); got != 4 {
		t.Fatalf("pool size = %d, want 4 after one-in-one-out", got)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, eviction must not confirm or advance round", n.CurrentRound(), n.Height())
	}

	// 旧轮次落选候选的交易顺序、投票记录与胜出/落选结果保持原样；
	// 查询历史不会把被挤出的 ret 重新变成等待投票。
	rc := mustCandidates(t, n, 1)
	if !reflect.DeepEqual(rc, rcBefore) {
		t.Fatalf("round 1 history changed by eviction:\nbefore=%+v\nafter =%+v", rcBefore, rc)
	}
	if info, _ := n.Tx(s.ret.ID()); info.Status != StatusDropped {
		t.Fatalf("history query revived evicted tx: status = %s, want dropped", info.Status)
	}
	if blk, _ := n.BlockAt(1); !reflect.DeepEqual(blk, blockBefore) {
		t.Fatalf("winning block changed:\nbefore=%+v\nafter =%+v", blockBefore, blk)
	}

	// 阶段三：池仍满，最低可淘汰排队交易费用为 41（fill/bump）。
	// 费用更低的新交易必须 pool-full：不留下交易历史，候选、账户、淘汰记录
	// 以及确认块关联全部保持不变。
	entriesBefore := capEntrySnapshot(n)
	accountsBefore := capAccountSnap(n, s.keys)
	low := NewTransaction(s.keys[2].priv, 2, []byte("too-cheap"), 5, 100)
	if r, err := n.Submit(low); reason(err) != ReasonPoolFull {
		t.Fatalf("submit low-fee got %+v %v, want %s", r, err, ReasonPoolFull)
	}
	if _, err := n.Tx(low.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx query = %v, want %s (no history left)", err, ReasonUnknownTx)
	}
	// 原排队交易保留，上一步被挤出的 ret 记录不被改动。
	if info, _ := n.Tx(s.back.ID()); info.Status != StatusQueued {
		t.Fatalf("back status = %s, want queued after pool-full rejection", info.Status)
	}
	if info, _ := n.Tx(s.ret.ID()); info.Status != StatusDropped ||
		info.DropReason != DropReasonPoolCapacity || info.DropRound != 2 {
		t.Fatalf("ret drop record changed by rejected submit: %+v", info)
	}
	if got := capEntrySnapshot(n); !reflect.DeepEqual(got, entriesBefore) {
		t.Fatalf("tx entries changed by pool-full rejection:\nbefore=%v\nafter =%v", entriesBefore, got)
	}
	if got := capAccountSnap(n, s.keys); !reflect.DeepEqual(got, accountsBefore) {
		t.Fatalf("accounts changed by pool-full rejection:\nbefore=%v\nafter =%v", accountsBefore, got)
	}
	if rc := mustCandidates(t, n, 1); !reflect.DeepEqual(rc, rcBefore) {
		t.Fatalf("round 1 history changed by pool-full rejection:\nbefore=%+v\nafter =%+v", rcBefore, rc)
	}
	if blk, _ := n.BlockAt(1); !reflect.DeepEqual(blk, blockBefore) {
		t.Fatalf("winning block changed by rejection:\nbefore=%+v\nafter =%+v", blockBefore, blk)
	}
	if got := n.st.poolSize(); got != 4 || n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("state shifted by rejection: size=%d round=%d height=%d, want 4/2/1",
			got, n.CurrentRound(), n.Height())
	}
}
