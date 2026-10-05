package consensus

import (
	"fmt"
	"reflect"
	"testing"
)

// 跨轮次容量淘汰回归：竞争候选在第 1 轮胜出后，节点仍保留旧轮次的候选列表、
// 交易顺序、投票者与胜出/落选结果；落选候选独有且未到期的交易回到排队。
// 这段历史引用不能继续保护已回池的交易——第 2 轮是否允许淘汰只取决于“当前是否
// 存在未决候选引用它”，而不是它是否曾经进入过候选。
//
// 固定场景（4 名验证者，单块上限与池容量均为 4）：
//
//	keys[0] shared1 费用 40：本地提议与竞争候选共享，随胜出块确认
//	keys[1] shared2 费用 20：本地提议与竞争候选共享，随胜出块确认
//	keys[2] loser    费用 100：仅本地提议引用，本地提议落选后回到排队
//	keys[3] repool   费用 30 ：仅本地提议引用，落选后回到排队；
//	           第 2 轮池满时是费用最低的唯一排队交易，将被费用严格更高的新交易挤出
//	keys[4] 第 2 轮挤出 repool 的新交易发送者
//	keys[5] 第 2 轮费用不足、被 pool-full 拒绝的新交易发送者
//
// 本地提议按费用打包全部四笔：[loser shared1 repool shared2]；竞争候选只登记
// 其中部分 [shared1 shared2] 并取得三票胜出，本地提议保留一票落选。
const (
	crFeeShared1 = 40
	crFeeShared2 = 20
	crFeeLoser   = 100
	crFeeRepool  = 30
	crFeeFill1   = 60
	crFeeFill2   = 70
	crFeeEvictor = 31 // 严格高于 repool(30)，低于其他全部排队交易
	crFeeCheap   = 5  // 低于挤出后池中的最低可淘汰费用
	crFeeHigh    = 200
	crExpiry     = uint64(100)
	crCapacity   = uint64(4)
)

type capRepoolScene struct {
	n       *Node
	dir     string
	keys    []testKey
	local   ProposalView
	altID   string
	shared1 *Transaction
	shared2 *Transaction
	loser   *Transaction
	repool  *Transaction
}

func newCapRepoolScene(t *testing.T) *capRepoolScene {
	t.Helper()
	keys := genKeys(t, 6)
	vals := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		vals[i] = keys[i].pub
	}
	dir := t.TempDir()
	n, err := New(dir, Config{
		Seed: []byte("cap-repool-history-scene"), Validators: vals,
		MaxTxsPerBlock: crCapacity, PoolCapacity: crCapacity,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &capRepoolScene{n: n, dir: dir, keys: keys}
	s.shared1 = NewTransaction(keys[0].priv, 1, []byte("shared-1"), crFeeShared1, crExpiry)
	s.shared2 = NewTransaction(keys[1].priv, 1, []byte("shared-2"), crFeeShared2, crExpiry)
	s.loser = NewTransaction(keys[2].priv, 1, []byte("loser-only"), crFeeLoser, crExpiry)
	s.repool = NewTransaction(keys[3].priv, 1, []byte("repool"), crFeeRepool, crExpiry)
	for _, tx := range []*Transaction{s.shared1, s.shared2, s.loser, s.repool} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 本地提议按费用从高到低打包全部四笔（各账户序号均为 1，不存在序号约束）。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	wantLocal := []string{s.loser.ID(), s.shared1.ID(), s.repool.ID(), s.shared2.ID()}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint(wantLocal) {
		t.Fatalf("local proposal = %v, want %v", local.TxIDs, wantLocal)
	}
	s.local = local

	// 竞争候选只包含其中两笔共享交易。
	s.altID = registerMust(t, n, 1, []string{s.shared1.ID(), s.shared2.ID()})
	if s.altID == local.BlockID {
		t.Fatal("competing candidate must have a distinct block id")
	}

	// 本地提议（落选方）保留一票；竞争候选随后取得三票，四人名单下第三票立即确认。
	if res, err := n.Vote(keys[3].pub, 1, local.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("vote for local proposal: %+v %v", res, err)
	}
	for i := 0; i < 2; i++ {
		if res, err := n.Vote(keys[i].pub, 1, s.altID); err != nil || !res.Counted || res.Confirmed {
			t.Fatalf("vote %d for competing candidate must not confirm yet: %+v %v", i+1, res, err)
		}
	}
	res, err := n.Vote(keys[2].pub, 1, s.altID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("third vote for competing candidate should confirm: %+v %v", res, err)
	}

	s.assertResolved(t)
	return s
}

// assertResolved 固定竞争候选确认后的衔接形态：共享交易关联胜出块，落选独有
// 交易未到期回到排队，账户确认序号各归其位，旧轮次候选/投票/结果完整保留，
// 而新轮次尚无任何候选引用回池交易。
func (s *capRepoolScene) assertResolved(t *testing.T) {
	t.Helper()
	n := s.n
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1 after confirm", n.CurrentRound(), n.Height())
	}

	// 共享交易随胜出块确认，区块即竞争候选标识。
	for _, tx := range []*Transaction{s.shared1, s.shared2} {
		info := mustTx(t, n, tx.ID())
		if info.Status != StatusConfirmed || info.BlockID != s.altID || info.BlockHeight != 1 {
			t.Fatalf("shared tx %s = %+v, want confirmed in competing block", tx.ID(), info)
		}
	}
	// 落选候选独有且未到期的交易回到排队。
	for _, tx := range []*Transaction{s.loser, s.repool} {
		info := mustTx(t, n, tx.ID())
		if info.Status != StatusQueued || info.BlockID != "" || info.ReplacedBy != "" {
			t.Fatalf("losing-only tx %s = %+v, want plain queued without links", tx.ID(), info)
		}
	}

	// 胜出账户序号推进到 1 且待处理清空；落选账户保持原值 0，独有交易等待打包。
	assertAccountQueue(t, n, s.keys[0].pub, 1, map[string]string{})
	assertAccountQueue(t, n, s.keys[1].pub, 1, map[string]string{})
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.loser.ID(): "waiting-pack"})
	assertAccountQueue(t, n, s.keys[3].pub, 0, map[string]string{s.repool.ID(): "waiting-pack"})

	// 确认块：连续高度 1、第 1 轮、空前块标识，交易顺序取胜出候选。
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != s.altID || blk.Height != 1 || blk.Round != 1 || blk.PreviousID != "" ||
		fmt.Sprint(blk.TxIDs) != fmt.Sprint([]string{s.shared1.ID(), s.shared2.ID()}) {
		t.Fatalf("confirmed block wrong: %+v", blk)
	}
	if latest, ok := n.LatestBlock(); !ok || latest.ID != s.altID {
		t.Fatalf("latest block = %+v ok=%v, want competing block", latest, ok)
	}

	// 旧轮次仍可查：候选列表、投票者与胜出/落选结果保持原样，四名验证者均已投票。
	rc := mustCandidates(t, n, 1)
	if !rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords || len(rc.Candidates) != 2 || len(rc.Unvoted) != 0 {
		t.Fatalf("round 1 history wrong: %+v", rc)
	}
	alt := findCandidate(t, n, 1, s.altID)
	if alt.Result != CandidateWon || alt.Local {
		t.Fatalf("competing candidate = %+v, want won non-local", alt)
	}
	if fmt.Sprint(alt.TxIDs) != fmt.Sprint([]string{s.shared1.ID(), s.shared2.ID()}) {
		t.Fatalf("winner tx order = %v, want [shared1 shared2]", alt.TxIDs)
	}
	if got := capRepoolVotersHex(alt.Voters); !reflect.DeepEqual(got, capRepoolKeyHex(s.keys, 0, 1, 2)) {
		t.Fatalf("winner voters = %v, want keys 0,1,2", got)
	}
	lost := findCandidate(t, n, 1, s.local.BlockID)
	if lost.Result != CandidateLost || !lost.Local {
		t.Fatalf("local candidate = %+v, want lost local", lost)
	}
	if fmt.Sprint(lost.TxIDs) != fmt.Sprint([]string{s.loser.ID(), s.shared1.ID(), s.repool.ID(), s.shared2.ID()}) {
		t.Fatalf("losing candidate tx order = %v, want [loser shared1 repool shared2]", lost.TxIDs)
	}
	if got := capRepoolVotersHex(lost.Voters); !reflect.DeepEqual(got, capRepoolKeyHex(s.keys, 3)) {
		t.Fatalf("losing candidate voters = %v, want key 3", got)
	}

	// 新轮次尚未产生本地提议，也就没有任何未决候选再次引用回池交易。
	if _, err := n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("round 2 candidates = %v, want %s (no pending candidate yet)", err, ReasonUnknownRound)
	}
}

// fillFreedSlots 用费用高于回池交易 repool 的新交易填满确认释放的两个位置。
// 此时仍有空位：两笔都正常入池，提交结果不带挤出或替换标识，回池交易保留。
func (s *capRepoolScene) fillFreedSlots(t *testing.T) (*Transaction, *Transaction) {
	t.Helper()
	n := s.n
	f1 := NewTransaction(s.keys[0].priv, 2, []byte("fill-1"), crFeeFill1, crExpiry)
	f2 := NewTransaction(s.keys[1].priv, 2, []byte("fill-2"), crFeeFill2, crExpiry)
	for _, tx := range []*Transaction{f1, f2} {
		res, err := n.Submit(tx)
		if err != nil {
			t.Fatalf("freed slot must accept tx without eviction: %v", err)
		}
		if res.TxID != tx.ID() || res.EvictedID != "" || res.ReplacedID != "" {
			t.Fatalf("freed-slot submit = %+v, want plain accept with no eviction/replace", res)
		}
	}
	// 回池的两笔交易仍原样排队等待打包。
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.loser.ID(): "waiting-pack"})
	assertAccountQueue(t, n, s.keys[3].pub, 0, map[string]string{s.repool.ID(): "waiting-pack"})
	// 填充交易排在各自胜出账户的已确认序号 1 之后，无缺口。
	assertAccountQueue(t, n, s.keys[0].pub, 1, map[string]string{f1.ID(): "waiting-pack"})
	assertAccountQueue(t, n, s.keys[1].pub, 1, map[string]string{f2.ID(): "waiting-pack"})
	return f1, f2
}

// capRepoolVotersHex 把候选投票者公钥转为排序后的十六进制列表，便于整体比较。
func capRepoolVotersHex(voters [][]byte) []string {
	out := make([]string, 0, len(voters))
	for _, v := range voters {
		out = append(out, fmt.Sprintf("%x", v))
	}
	sortStrings(out)
	return out
}

// capRepoolKeyHex 返回指定下标的验证者公钥十六进制排序列表。
func capRepoolKeyHex(keys []testKey, idx ...int) []string {
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, fmt.Sprintf("%x", keys[i].pub))
	}
	sortStrings(out)
	return out
}

// 主回归：竞争候选胜出后的下一轮提交里，曾经进入落选候选的回池交易不再受保护。
// 填满确认释放的位置后池再次满员，repool 是唯一最低费排队交易，被费用严格更高的
// 新交易挤出；淘汰记录落在新轮次，账户、历史候选与确认块关联一律不变。
func TestCapacityRepooledTxEvictableAfterCompetingConfirm(t *testing.T) {
	s := newCapRepoolScene(t)
	n := s.n
	f1, f2 := s.fillFreedSlots(t)

	// 淘汰发生前先固定旧轮次历史与确认块，淘汰后必须逐字节保持。
	rcBefore := mustCandidates(t, n, 1)
	blockBefore, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}

	// 当前排队：loser(100)、f1(60)、f2(70)、repool(30)；repool 是唯一最低费对象。
	// 新交易费用 31 严格更高，必须接收并挤出 repool，而不是受其旧候选引用保护。
	x := NewTransaction(s.keys[4].priv, 1, []byte("evictor"), crFeeEvictor, crExpiry)
	res, err := n.Submit(x)
	if err != nil {
		t.Fatalf("higher-fee tx must evict the repooled tx in the new round: %v", err)
	}
	if res.TxID != x.ID() || res.EvictedID != s.repool.ID() || res.ReplacedID != "" {
		t.Fatalf("submit result = %+v, want accept with evicted repool and no replace link", res)
	}

	// 被挤出的回池交易：dropped / pool-capacity / 发生轮次为本次提交所在的新轮次 2，
	// 完整内容仍可查，不带确认块或替换关联。
	info, err := n.Tx(s.repool.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 2 {
		t.Fatalf("repool tx after eviction = %+v, want dropped/pool-capacity/round 2", info)
	}
	if info.BlockHeight != 0 || info.BlockID != "" || info.ReplacedBy != "" {
		t.Fatalf("dropped repool tx must not carry block/replace links: %+v", info)
	}
	assertTxMatches(t, info.Tx, s.repool)

	// 新交易排队等待打包。
	if xInfo, _ := n.Tx(x.ID()); xInfo.Status != StatusQueued {
		t.Fatalf("evictor tx status = %s, want queued", xInfo.Status)
	}
	assertAccountQueue(t, n, s.keys[4].pub, 0, map[string]string{x.ID(): "waiting-pack"})

	// repool 发送者：待处理列表移除旧交易，已确认序号不因淘汰推进（仍为 0，无缺口）。
	acct := n.Account(s.keys[3].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 0 || len(acct.Pending) != 0 {
		t.Fatalf("repool sender account = %+v, want confirmed 0 gap 0 no pending", acct)
	}
	// 其他排队交易保留原有顺序与内容；胜出账户的已确认序号保持 1。
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.loser.ID(): "waiting-pack"})
	assertAccountQueue(t, n, s.keys[0].pub, 1, map[string]string{f1.ID(): "waiting-pack"})
	assertAccountQueue(t, n, s.keys[1].pub, 1, map[string]string{f2.ID(): "waiting-pack"})
	assertTxMatches(t, mustTx(t, n, s.loser.ID()).Tx, s.loser)
	assertTxMatches(t, mustTx(t, n, f1.ID()).Tx, f1)
	assertTxMatches(t, mustTx(t, n, f2.ID()).Tx, f2)

	// 共享交易仍是胜出块关联，确认序号不被淘汰改动。
	for _, tx := range []*Transaction{s.shared1, s.shared2} {
		win := mustTx(t, n, tx.ID())
		if win.Status != StatusConfirmed || win.BlockID != s.altID || win.BlockHeight != 1 {
			t.Fatalf("winner tx %s lost confirmation link: %+v", tx.ID(), win)
		}
	}

	// 旧轮次落选候选的交易顺序与投票记录保持原样，胜出候选的确认块关联不变；
	// 查询历史不会把已被挤出的 repool 重新变成等待投票。
	rcAfter := mustCandidates(t, n, 1)
	if !reflect.DeepEqual(rcAfter, rcBefore) {
		t.Fatalf("round 1 history changed by eviction:\nbefore=%+v\nafter =%+v", rcBefore, rcAfter)
	}
	lost := findCandidate(t, n, 1, s.local.BlockID)
	if lost.Result != CandidateLost ||
		fmt.Sprint(lost.TxIDs) != fmt.Sprint([]string{s.loser.ID(), s.shared1.ID(), s.repool.ID(), s.shared2.ID()}) {
		t.Fatalf("losing candidate altered by eviction: %+v", lost)
	}
	if got := capRepoolVotersHex(lost.Voters); !reflect.DeepEqual(got, capRepoolKeyHex(s.keys, 3)) {
		t.Fatalf("losing candidate voters altered: %v", got)
	}
	if d, err := n.BlockAt(1); err != nil || !reflect.DeepEqual(d, blockBefore) {
		t.Fatalf("confirmed block changed by eviction: %+v %v", d, err)
	}
	if got := mustTx(t, n, s.repool.ID()).Status; got != StatusDropped {
		t.Fatalf("history listing revived evicted tx to %s, want dropped", got)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("eviction must not advance round/height: round=%d height=%d", n.CurrentRound(), n.Height())
	}
}

// 满池时若新交易费用低于最低可淘汰费用，仍以 pool-full 拒绝：不留交易历史，
// 候选历史、确认块、账户与既有淘汰记录全部保持操作前结果。
func TestCapacityRepooledLowFeeRejectedWhenFull(t *testing.T) {
	s := newCapRepoolScene(t)
	n := s.n
	s.fillFreedSlots(t)
	x := NewTransaction(s.keys[4].priv, 1, []byte("evictor"), crFeeEvictor, crExpiry)
	res, err := n.Submit(x)
	if err != nil || res.EvictedID != s.repool.ID() {
		t.Fatalf("setup eviction: %+v %v", res, err)
	}

	// 拒绝前固定全部对外可见状态。
	beforeEntries := capEntrySnapshot(n)
	beforeAccounts := capAccountSnap(n, s.keys)
	beforeRC := mustCandidates(t, n, 1)
	beforeBlock, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}

	// 挤出后最低可淘汰交易是费用 31 的 x；费用 5 的新交易严格更低，必须拒绝。
	cheap := NewTransaction(s.keys[5].priv, 1, []byte("too-cheap"), crFeeCheap, crExpiry)
	res, err = n.Submit(cheap)
	if reason(err) != ReasonPoolFull {
		t.Fatalf("low-fee submit got %v, want %s", err, ReasonPoolFull)
	}
	if res != nil {
		t.Fatalf("rejected submit returned %+v, want nil", res)
	}
	// 拒绝不留任何交易历史。
	if _, err := n.Tx(cheap.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx query = %v, want %s", err, ReasonUnknownTx)
	}

	// 池与全部历史视图保持操作前结果。
	if got := capEntrySnapshot(n); !reflect.DeepEqual(got, beforeEntries) {
		t.Fatalf("entries changed by rejected submit:\nbefore=%v\nafter =%v", beforeEntries, got)
	}
	if got := capAccountSnap(n, s.keys); !reflect.DeepEqual(got, beforeAccounts) {
		t.Fatalf("accounts changed by rejected submit:\nbefore=%v\nafter =%v", beforeAccounts, got)
	}
	if got := mustCandidates(t, n, 1); !reflect.DeepEqual(got, beforeRC) {
		t.Fatalf("round 1 history changed by rejected submit:\nbefore=%+v\nafter =%+v", beforeRC, got)
	}
	if got, err := n.BlockAt(1); err != nil || !reflect.DeepEqual(got, beforeBlock) {
		t.Fatalf("confirmed block changed by rejected submit: %+v %v", got, err)
	}
	// 既有淘汰记录不被覆盖，挤出者 x 仍在排队。
	if info := mustTx(t, n, s.repool.ID()); info.Status != StatusDropped || info.DropRound != 2 ||
		info.DropReason != DropReasonPoolCapacity {
		t.Fatalf("previous drop record changed: %+v", info)
	}
	if info := mustTx(t, n, x.ID()); info.Status != StatusQueued {
		t.Fatalf("evictor tx changed to %s, want queued", info.Status)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("rejection must not advance round/height: round=%d height=%d", n.CurrentRound(), n.Height())
	}
}

// 重启恢复后，回池交易的淘汰记录、新轮次的排队形态与旧轮次候选历史都保持原样；
// 后续容量决策仍只看当前在池排队交易：已 dropped 的 repool 既不占位也不被二次
// 当作保护对象，费用最高的新交易挤出的是当前最低费排队交易 x。
func TestCapacityRepooledEvictionPersistsAcrossReopen(t *testing.T) {
	s := newCapRepoolScene(t)
	n := s.n
	f1, f2 := s.fillFreedSlots(t)
	x := NewTransaction(s.keys[4].priv, 1, []byte("evictor"), crFeeEvictor, crExpiry)
	if res, err := n.Submit(x); err != nil || res.EvictedID != s.repool.ID() {
		t.Fatalf("setup eviction: %+v %v", res, err)
	}

	r, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.CurrentRound() != 2 || r.Height() != 1 || r.Config().PoolCapacity != crCapacity {
		t.Fatalf("restored node = round %d height %d cap %d, want 2/1/%d",
			r.CurrentRound(), r.Height(), r.Config().PoolCapacity, crCapacity)
	}

	// 淘汰记录与完整内容恢复。
	dropped, err := r.Tx(s.repool.ID())
	if err != nil {
		t.Fatal(err)
	}
	if dropped.Status != StatusDropped || dropped.DropReason != DropReasonPoolCapacity || dropped.DropRound != 2 {
		t.Fatalf("restored drop record wrong: %+v", dropped)
	}
	if dropped.BlockHeight != 0 || dropped.BlockID != "" || dropped.ReplacedBy != "" {
		t.Fatalf("restored dropped tx carries links: %+v", dropped)
	}
	assertTxMatches(t, dropped.Tx, s.repool)

	// 其余交易状态恢复：共享交易仍确认，回池/填充/挤出交易仍排队。
	for _, tx := range []*Transaction{s.shared1, s.shared2} {
		if info, _ := r.Tx(tx.ID()); info.Status != StatusConfirmed || info.BlockID != s.altID || info.BlockHeight != 1 {
			t.Fatalf("restored winner tx %s wrong: %+v", tx.ID(), info)
		}
	}
	for _, tx := range []*Transaction{s.loser, f1, f2, x} {
		if info, _ := r.Tx(tx.ID()); info.Status != StatusQueued {
			t.Fatalf("restored queued tx %s = %s, want queued", tx.ID(), info.Status)
		}
	}
	assertAccountQueue(t, r, s.keys[3].pub, 0, map[string]string{})
	assertAccountQueue(t, r, s.keys[4].pub, 0, map[string]string{x.ID(): "waiting-pack"})

	// 旧轮次候选历史（顺序、投票者、胜出/落选）恢复。
	rc := mustCandidates(t, r, 1)
	if !rc.Ended || rc.UnconfirmedEnd || len(rc.Candidates) != 2 {
		t.Fatalf("restored round 1 history wrong: %+v", rc)
	}
	alt := findCandidate(t, r, 1, s.altID)
	if alt.Result != CandidateWon ||
		fmt.Sprint(alt.TxIDs) != fmt.Sprint([]string{s.shared1.ID(), s.shared2.ID()}) ||
		!reflect.DeepEqual(capRepoolVotersHex(alt.Voters), capRepoolKeyHex(s.keys, 0, 1, 2)) {
		t.Fatalf("restored winner candidate wrong: %+v", alt)
	}
	lost := findCandidate(t, r, 1, s.local.BlockID)
	if lost.Result != CandidateLost ||
		fmt.Sprint(lost.TxIDs) != fmt.Sprint([]string{s.loser.ID(), s.shared1.ID(), s.repool.ID(), s.shared2.ID()}) ||
		!reflect.DeepEqual(capRepoolVotersHex(lost.Voters), capRepoolKeyHex(s.keys, 3)) {
		t.Fatalf("restored losing candidate wrong: %+v", lost)
	}

	// 恢复后容量决策仍只取决于当前在池排队交易：低费新交易依旧 pool-full。
	cheap := NewTransaction(s.keys[5].priv, 1, []byte("too-cheap"), crFeeCheap, crExpiry)
	if _, err := r.Submit(cheap); reason(err) != ReasonPoolFull {
		t.Fatalf("restored low-fee submit got %v, want %s", err, ReasonPoolFull)
	}
	if _, err := r.Tx(cheap.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected cheap tx left history: %v", err)
	}
	// 高费新交易挤出当前最低费排队交易 x；已 dropped 的 repool 不复活、不参与。
	z := NewTransaction(s.keys[5].priv, 1, []byte("high-bid"), crFeeHigh, crExpiry)
	res, err := r.Submit(z)
	if err != nil {
		t.Fatalf("high-fee tx after reopen should evict current lowest queued tx: %v", err)
	}
	if res.TxID != z.ID() || res.EvictedID != x.ID() || res.ReplacedID != "" {
		t.Fatalf("post-reopen eviction = %+v, want evicted x only", res)
	}
	if info := mustTx(t, r, x.ID()); info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 2 {
		t.Fatalf("x after post-reopen eviction = %+v, want dropped/pool-capacity/round 2", info)
	}
	// 旧的 repool 淘汰记录保持原样，不被新一次淘汰覆盖或重新计入。
	if info := mustTx(t, r, s.repool.ID()); info.Status != StatusDropped || info.DropRound != 2 {
		t.Fatalf("older dropped repool tx changed: %+v", info)
	}
	if info := mustTx(t, r, z.ID()); info.Status != StatusQueued {
		t.Fatalf("new high-fee tx = %s, want queued", info.Status)
	}
}
