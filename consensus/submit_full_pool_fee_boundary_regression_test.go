package consensus

import (
	"fmt"
	"reflect"
	"testing"
)

// 本文件为“满池时接收新交易”的费用比较补充回归保障：现有容量用例只覆盖普通
// 费用下的淘汰（capacity_test.go、capacity_protected_eviction_regression_test.go），
// 极大费用的覆盖集中在本地提议排序（propose_fee_boundary_regression_test.go）。
// 这里钉住满池竞争在费用取 0、2^63-1、2^63、2^64-1 时的实际业务结果：
// 新交易只与优先级最低的一笔排队交易竞争，费用按无符号 64 位整数真实大小比较，
// 同费时按完整交易标识字典序决定去留，且整个操作只改变交易池。
// 保障一律通过提交结果与公开查询（Tx / Account / CurrentRound / Height）表达，
// 不只验证费用比较本身。
//
// 回归针对的典型错误实现：把费用当 int64 比较（2^63 被当成负数而输给 2^63-1）、
// 有符号差值判大小（最大费用与 0 的极端差距反转或溢出被判相同）、把 0 费用当成
// “没有可淘汰对象”而直接 pool-full、同费时忽略标识规则（尤其在最大费用下，
// 不存在更大的合法费用，接收与否只能由标识关系决定）。

// feeBoundaryPool 是“满池 + 全部排队”的固定场景：容量恰好被一池排队交易占满，
// 每笔交易来自不同账户（序号均为 1、签名有效、未到期），均未被任何未决候选引用
// （场景不产生提议），因此每一笔都是合法的可淘汰对象，唯一可被淘汰的是其中
// 优先级最低者（费用最低，同费取标识字典序最大）。
type feeBoundaryPool struct {
	n    *Node
	keys []testKey      // 全部相关账户：池内交易发送者与历次新交易发送者
	pool []*Transaction // 当前应处于 queued 的池内交易
}

func newFeeBoundaryPool(t *testing.T, fees []uint64) *feeBoundaryPool {
	t.Helper()
	keys := genKeys(t, len(fees))
	n, _ := newTestNodeCap(t, keys, 10, uint64(len(fees)))
	s := &feeBoundaryPool{n: n, keys: keys}
	for i, fee := range fees {
		tx := NewTransaction(keys[i].priv, 1, []byte(fmt.Sprintf("pool-%d", i)), fee, 100)
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit fee %d: %v", fee, err)
		}
		s.pool = append(s.pool, tx)
	}
	// 场景固定：全部排队等待打包、池恰好满、轮次 1、无确认块。
	for _, tx := range s.pool {
		info, err := n.Tx(tx.ID())
		if err != nil || info.Status != StatusQueued {
			t.Fatalf("setup: tx %s = %+v %v, want queued", tx.ID(), info, err)
		}
	}
	if got := s.pendingTotal(t); got != len(fees) {
		t.Fatalf("setup: pending total = %d, want %d (pool must be exactly full)", got, len(fees))
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("setup: round %d height %d, want round 1 height 0", n.CurrentRound(), n.Height())
	}
	return s
}

// pendingTotal 通过公开的账户查询汇总池内待处理交易数。
func (s *feeBoundaryPool) pendingTotal(t *testing.T) int {
	t.Helper()
	total := 0
	for _, k := range s.keys {
		total += len(s.n.Account(k.pub).Pending)
	}
	return total
}

// assertAccepted 提交一笔来自全新账户的合法交易（签名有效、未到期、序号合法、
// 首次提交，不替换任何旧交易）并断言满池竞争成功：结果准确关联新旧交易标识且
// 无替换关联，新交易排队，受害者带完整淘汰记录并退出原账户待处理列表，其余
// 池内交易与账户保持原样，已确认序号、轮次与确认高度均不推进。
// 成功后新交易及其发送者并入场景。
func (s *feeBoundaryPool) assertAccepted(t *testing.T, key testKey, tx, victim *Transaction) {
	t.Helper()
	n := s.n
	s.keys = append(s.keys, key)
	beforeEntries := capEntrySnapshot(n)
	beforeAccounts := capAccountSnap(n, s.keys)

	res, err := n.Submit(tx)
	if err != nil {
		t.Fatalf("submit fee %d must be admitted to the full pool by evicting fee %d: %v", tx.Fee, victim.Fee, err)
	}
	if res.TxID != tx.ID() || res.EvictedID != victim.ID() || res.ReplacedID != "" {
		t.Fatalf("submit result = %+v, want TxID %s EvictedID %s and no replacement", res, tx.ID(), victim.ID())
	}

	// 新交易：排队等待打包，内容完整保留，不带任何淘汰/替换/确认关联。
	info, err := n.Tx(tx.ID())
	if err != nil || info.Status != StatusQueued {
		t.Fatalf("new tx = %+v %v, want queued", info, err)
	}
	if info.DropReason != "" || info.DropRound != 0 || info.ReplacedBy != "" || info.BlockHeight != 0 || info.BlockID != "" {
		t.Fatalf("queued tx must not carry drop/replace/block links: %+v", info)
	}
	assertTxMatches(t, info.Tx, tx)
	assertAccountQueue(t, n, key.pub, 0, map[string]string{tx.ID(): "waiting-pack"})

	// 被挤出的交易：dropped / pool-capacity / 发生轮次 1，完整内容保留，
	// 不带确认块或替换关联，并退出原账户的待处理列表（已确认序号不推进）。
	vinfo, err := n.Tx(victim.ID())
	if err != nil {
		t.Fatal(err)
	}
	if vinfo.Status != StatusDropped || vinfo.DropReason != DropReasonPoolCapacity || vinfo.DropRound != 1 {
		t.Fatalf("evicted tx = %+v, want dropped/pool-capacity/round 1", vinfo)
	}
	if vinfo.BlockHeight != 0 || vinfo.BlockID != "" || vinfo.ReplacedBy != "" {
		t.Fatalf("evicted tx must not carry block/replace links: %+v", vinfo)
	}
	assertTxMatches(t, vinfo.Tx, victim)
	assertAccountQueue(t, n, victim.Sender, 0, map[string]string{})

	// 其余条目状态与关联记录不变；旁观者账户与提交前完全一致。
	afterEntries := capEntrySnapshot(n)
	for id, before := range beforeEntries {
		if id == victim.ID() {
			continue
		}
		if afterEntries[id] != before {
			t.Fatalf("tx %s changed by eviction: before=%+v after=%+v", id, before, afterEntries[id])
		}
	}
	afterAccounts := capAccountSnap(n, s.keys)
	victimHex := victim.SenderHex()
	newHex := fmt.Sprintf("%x", key.pub)
	for _, k := range s.keys {
		hex := fmt.Sprintf("%x", k.pub)
		if hex == victimHex || hex == newHex {
			continue
		}
		if afterAccounts[hex] != beforeAccounts[hex] {
			t.Fatalf("bystander account %s changed:\nbefore=%s\nafter =%s", hex, beforeAccounts[hex], afterAccounts[hex])
		}
	}
	// 其余池内交易保持排队、内容原样；新交易补入场景池。
	kept := make([]*Transaction, 0, len(s.pool))
	for _, p := range s.pool {
		if p.ID() == victim.ID() {
			continue
		}
		pinfo := mustTx(t, n, p.ID())
		if pinfo.Status != StatusQueued {
			t.Fatalf("pool tx %s status = %s, want queued", p.ID(), pinfo.Status)
		}
		assertTxMatches(t, pinfo.Tx, p)
		kept = append(kept, p)
	}
	s.pool = append(kept, tx)

	// 挤一出一，池仍恰好满；操作只改变交易池，不推进轮次与确认高度。
	if got := s.pendingTotal(t); got != len(s.pool) {
		t.Fatalf("pending total after eviction = %d, want %d (pool must stay exactly full)", got, len(s.pool))
	}
	if n.CurrentRound() != 1 {
		t.Fatalf("round = %d, eviction must not advance the round", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, eviction must not confirm a block", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("eviction must not leave a confirmed block")
	}
}

// assertRejected 断言满池提交被 pool-full 拒绝：不返回结果、不留被拒交易的
// 任何记录，池内交易状态与全部账户队列保持提交前一致，轮次与确认高度不变。
func (s *feeBoundaryPool) assertRejected(t *testing.T, tx *Transaction) {
	t.Helper()
	n := s.n
	beforeEntries := capEntrySnapshot(n)
	beforeAccounts := capAccountSnap(n, s.keys)

	res, err := n.Submit(tx)
	if reason(err) != ReasonPoolFull {
		t.Fatalf("submit fee %d got %v, want %s", tx.Fee, err, ReasonPoolFull)
	}
	if res != nil {
		t.Fatalf("rejected submit returned result %+v, want nil", res)
	}
	// 不留被拒交易的记录：按标识查询不存在，发送者账户无待处理交易。
	if _, err := n.Tx(tx.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx query = %v, want %s", err, ReasonUnknownTx)
	}
	assertAccountQueue(t, n, tx.Sender, 0, map[string]string{})

	// 池内交易与账户队列完全冻结：没有条目状态变化，没有新增淘汰记录。
	if got := capEntrySnapshot(n); !reflect.DeepEqual(got, beforeEntries) {
		t.Fatalf("entries changed by rejected submit:\nbefore=%v\nafter =%v", beforeEntries, got)
	}
	if got := capAccountSnap(n, s.keys); !reflect.DeepEqual(got, beforeAccounts) {
		t.Fatalf("accounts changed by rejected submit:\nbefore=%v\nafter =%v", beforeAccounts, got)
	}
	for _, p := range s.pool {
		info := mustTx(t, n, p.ID())
		if info.Status != StatusQueued || info.DropReason != "" || info.DropRound != 0 {
			t.Fatalf("pool tx %s = %+v, want untouched queued", p.ID(), info)
		}
	}
	if got := s.pendingTotal(t); got != len(s.pool) {
		t.Fatalf("pending total after rejection = %d, want %d (rejection must not free a slot)", got, len(s.pool))
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("rejection must not advance round/height: round %d height %d", n.CurrentRound(), n.Height())
	}
}

// 费用覆盖 0、2^63-1、2^63、2^64-1 的满池：0 费用是合法淘汰对象，不能被
// “费用为 0 即无可淘汰对象”的实现忽略。费用 1 的新交易只挤出 0 费用交易，
// 三笔高费交易（含最大费用）原样保留——最大费用与 0 的巨大差距不会让它们
// 被误选为受害者，也不会让高低关系反转。同费为 0 且标识更大的新交易必须
// pool-full：0 费用新交易并不能凭“费用为 0”被特殊放行。
func TestFullPoolSubmitEvictsZeroFeeAmongBoundaryFees(t *testing.T) {
	s := newFeeBoundaryPool(t, []uint64{feeZero, feeIntMax, feeHighBit, feeUintMax})
	zero := s.pool[0] // 费用 0，唯一可被淘汰的最低优先级交易

	// 同费（0）但标识更大：不能进入。
	nk1 := genKeys(t, 1)[0]
	loser := craftTxWithID(t, nk1.priv, 1, feeZero, zero.ID(), false)
	if loser.ID() <= zero.ID() {
		t.Fatal("test setup: crafted id must be larger than the zero-fee victim")
	}
	s.assertRejected(t, loser)

	// 费用 1 严格高于 0：挤出 0 费用交易，且只挤出它。
	nk2 := genKeys(t, 1)[0]
	win := NewTransaction(nk2.priv, 1, []byte("fee-one"), 1, 100)
	s.assertAccepted(t, nk2, win, zero)
}

// 0 费用同费竞争：新交易标识更小时必须接收——0 是合法费用，既不是“没有费用”，
// 也不会在同费比较中被特殊化；池内 0 费用交易被正常淘汰并留下完整淘汰记录。
func TestFullPoolSubmitZeroFeeTieSmallerIDAccepted(t *testing.T) {
	s := newFeeBoundaryPool(t, []uint64{feeZero, 7})
	zero := s.pool[0] // 费用 0，唯一可被淘汰的最低优先级交易

	nk := genKeys(t, 1)[0]
	win := craftTxWithID(t, nk.priv, 1, feeZero, zero.ID(), true)
	if win.ID() >= zero.ID() {
		t.Fatal("test setup: crafted id must be smaller than the zero-fee victim")
	}
	s.assertAccepted(t, nk, win, zero)
}

// 跨过 2^63 的高低关系：费用 2^63 必须严格高于 2^63-1（按 int64 比较会把 2^63
// 当成负数而错误拒绝）。满池时费用 2^63 的新交易挤出费用 2^63-1 的受害者，
// 费用同为 2^63 与 2^64-1 的池内交易不受影响；随后池中两笔 2^63 同费，标识
// 较大者成为新的受害者，最大费用新交易再凭真实费用挤出它——最大费用与 2^63
// 的差距不会被误判为相同或反转。
func TestFullPoolSubmitFeeAcrossInt63Boundary(t *testing.T) {
	s := newFeeBoundaryPool(t, []uint64{feeIntMax, feeHighBit, feeUintMax})
	intMax := s.pool[0]  // 费用 2^63-1，唯一可被淘汰的最低优先级交易
	highBit := s.pool[1] // 费用 2^63

	// 同费 2^63-1、标识更大：不能进入。
	nk1 := genKeys(t, 1)[0]
	loser := craftTxWithID(t, nk1.priv, 1, feeIntMax, intMax.ID(), false)
	if loser.ID() <= intMax.ID() {
		t.Fatal("test setup: crafted id must be larger than the victim")
	}
	s.assertRejected(t, loser)

	// 费用 2^63 严格高于 2^63-1：挤出受害者，另外两笔保留。
	nk2 := genKeys(t, 1)[0]
	mid := NewTransaction(nk2.priv, 1, []byte("fee-high-bit-new"), feeHighBit, 100)
	s.assertAccepted(t, nk2, mid, intMax)

	// 池中现有两笔 2^63：同费时标识较大者才是受害者；最大费用新交易挤出它。
	victim := highBit
	if mid.ID() > highBit.ID() {
		victim = mid
	}
	nk3 := genKeys(t, 1)[0]
	top := NewTransaction(nk3.priv, 1, []byte("fee-max-new"), feeUintMax, 100)
	s.assertAccepted(t, nk3, top, victim)
}

// 同费规则在最大费用下的保障：不存在比 2^64-1 更大的合法费用，接收与否只能
// 由同费标识关系决定。池内三笔最大费用交易中标识最大者优先被淘汰：标识更大
// 的新交易必须 pool-full，标识更小的新交易必须接收并只挤出该受害者。
func TestFullPoolSubmitMaxFeeTieBreakByTxID(t *testing.T) {
	s := newFeeBoundaryPool(t, []uint64{feeUintMax, feeUintMax, feeUintMax})

	// 三笔同费（均为最大值）：优先级最低的是标识最大者。
	victim := s.pool[0]
	for _, tx := range s.pool[1:] {
		if tx.ID() > victim.ID() {
			victim = tx
		}
	}

	// 标识比受害者更大：费用不可能更高，标识又不占优，必须拒绝。
	nk1 := genKeys(t, 1)[0]
	loser := craftTxWithID(t, nk1.priv, 1, feeUintMax, victim.ID(), false)
	if loser.ID() <= victim.ID() {
		t.Fatal("test setup: crafted id must be larger than the victim")
	}
	s.assertRejected(t, loser)

	// 标识比受害者更小：接收并只挤出受害者，其余两笔最大费用交易保留。
	nk2 := genKeys(t, 1)[0]
	win := craftTxWithID(t, nk2.priv, 1, feeUintMax, victim.ID(), true)
	if win.ID() >= victim.ID() {
		t.Fatal("test setup: crafted id must be smaller than the victim")
	}
	s.assertAccepted(t, nk2, win, victim)
}

// 费用较低时标识再小也不能抢占：受害者费用 2^63，新交易费用 2^63-1 或 0，
// 即使标识构造得更小也必须 pool-full，原有交易状态与账户队列不变；随后最大
// 费用新交易正常挤出受害者，证明拒绝确实源于费用不足而非场景异常。
func TestFullPoolSubmitLowerFeeCannotPreemptBySmallerID(t *testing.T) {
	s := newFeeBoundaryPool(t, []uint64{feeHighBit, feeUintMax})
	victim := s.pool[0] // 费用 2^63，唯一可被淘汰的最低优先级交易

	// 费用 2^63-1（低 1）且标识更小：仍不能抢占。
	nk1 := genKeys(t, 1)[0]
	justBelow := craftTxWithID(t, nk1.priv, 1, feeIntMax, victim.ID(), true)
	if justBelow.ID() >= victim.ID() {
		t.Fatal("test setup: crafted id must be smaller than the victim")
	}
	s.assertRejected(t, justBelow)

	// 费用 0 且标识更小：同样不能抢占。
	nk2 := genKeys(t, 1)[0]
	zero := craftTxWithID(t, nk2.priv, 1, feeZero, victim.ID(), true)
	if zero.ID() >= victim.ID() {
		t.Fatal("test setup: crafted id must be smaller than the victim")
	}
	s.assertRejected(t, zero)

	// 最大费用严格高于 2^63：正常挤出受害者，池内另一笔最大费用交易保留。
	nk3 := genKeys(t, 1)[0]
	win := NewTransaction(nk3.priv, 1, []byte("fee-max-new"), feeUintMax, 100)
	s.assertAccepted(t, nk3, win, victim)
}
