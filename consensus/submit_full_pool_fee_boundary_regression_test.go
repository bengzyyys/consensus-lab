package consensus

import (
	"testing"
)

// 本文件为“满池接收新交易时的费用比较”补充回归保障。普通费用下的淘汰见
// capacity_test.go，等待投票交易受保护的混合场景见
// capacity_protected_eviction_regression_test.go，而极大费用的 uint64 边界此前只在
// 本地提议排序（propose_fee_boundary_regression_test.go）中被钉住。这里专门钉住满池
// 接收入口：参与比较的交易全部签名有效、尚未到期、序号合法、首次提交且不替换任何
// 同发送者同序号旧交易，池内交易全部在排队、未被任何未决候选引用，节点设置恰好
// 占满的有限容量，且不产生提议——整个操作只应改变交易池。
//
// 回归针对的典型错误实现：把费用当 int64 比较（2^63 被视为负数，会让跨最高位的
// 高低关系倒置、选错可淘汰对象）、有符号差值判大小、把 0 费用交易当成不可淘汰或
// 不允许 0 费用新交易竞争、以及在满池时与不止一笔交易比较而误淘汰费用更高者。

// newFullQueuedPool 建立一个容量恰为 len(fees)、并被各账户一笔排队交易占满的节点。
// 每个账户只提交序号 1、到期轮次 100 的一笔有效签名交易；最后一个密钥保留给
// 随后提交的新交易，保证新交易是首次提交、不触发同序号替换。
func newFullQueuedPool(t *testing.T, fees []uint64, labels []string) (*Node, []testKey, []*Transaction) {
	t.Helper()
	if len(labels) != len(fees) {
		t.Fatalf("setup: %d fees but %d labels", len(fees), len(labels))
	}
	keys := genKeys(t, len(fees)+1)
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(t.TempDir(), Config{
		Seed: []byte("full-pool-fee-boundary"), Validators: vals,
		MaxTxsPerBlock: 10, PoolCapacity: uint64(len(fees)),
	})
	if err != nil {
		t.Fatal(err)
	}
	txs := make([]*Transaction, len(fees))
	for i := range fees {
		txs[i] = NewTransaction(keys[i].priv, 1, []byte(labels[i]), fees[i], 100)
		if !txs[i].Verify() {
			t.Fatalf("setup: pool tx %d signature must verify", i)
		}
		if _, err := n.Submit(txs[i]); err != nil {
			t.Fatalf("setup: submit pool tx %d: %v", i, err)
		}
	}
	if got := n.st.poolSize(); got != uint64(len(fees)) {
		t.Fatalf("setup: pool size = %d, want exactly %d (full)", got, len(fees))
	}
	return n, keys, txs
}

// assertEvictionBusiness 在一次成功的满池接收后核对完整业务结果，而不仅是费用比较：
// 提交结果关联准确的新旧标识；新交易排队等待打包；被挤出交易 dropped/pool-capacity/
// 发生轮次 1、完整内容保留且不带确认块或替换关联，并退出原账户待处理列表；
// 其余池内交易一律原样排队；池计数一出一入仍恰好满；轮次、确认高度与已确认序号不变。
func assertEvictionBusiness(t *testing.T, n *Node, keys []testKey,
	pool []*Transaction, victim, newTx *Transaction, res *SubmitResult,
) {
	t.Helper()
	if res.TxID != newTx.ID() || res.EvictedID != victim.ID() || res.ReplacedID != "" {
		t.Fatalf("submit result = %+v, want TxID=%s EvictedID=%s no replace link",
			res, newTx.ID(), victim.ID())
	}

	// 新交易：queued，完整内容与费用原样保留，账户显示等待打包。
	newInfo, err := n.Tx(newTx.ID())
	if err != nil {
		t.Fatalf("new tx query: %v", err)
	}
	if newInfo.Status != StatusQueued || newInfo.DropReason != "" || newInfo.DropRound != 0 {
		t.Fatalf("new tx = %+v, want plain queued", newInfo)
	}
	assertTxMatches(t, newInfo.Tx, newTx)
	assertAccountQueue(t, n, keys[len(pool)].pub, 0, map[string]string{
		newTx.ID(): "waiting-pack",
	})

	// 被挤出交易：dropped / pool-capacity / 轮次 1，完整内容保留，无确认块或替换关联。
	vInfo, err := n.Tx(victim.ID())
	if err != nil {
		t.Fatalf("victim query: %v", err)
	}
	if vInfo.Status != StatusDropped ||
		vInfo.DropReason != DropReasonPoolCapacity || vInfo.DropRound != 1 {
		t.Fatalf("victim = %+v, want dropped/pool-capacity/round 1", vInfo)
	}
	if vInfo.BlockHeight != 0 || vInfo.BlockID != "" || vInfo.ReplacedBy != "" {
		t.Fatalf("dropped victim must not carry block/replace links: %+v", vInfo)
	}
	assertTxMatches(t, vInfo.Tx, victim)

	// 逐账户核对：受害者退出原账户待处理列表，其余交易保持等待打包；
	// 所有已确认序号仍为 0（淘汰不推进序号）。
	for i, old := range pool {
		acct := n.Account(keys[i].pub)
		if acct.ConfirmedSequence != 0 {
			t.Fatalf("account %d confirmed sequence = %d, want 0", i, acct.ConfirmedSequence)
		}
		wantNotes := map[string]string{}
		if old.ID() != victim.ID() {
			wantNotes[old.ID()] = "waiting-pack"
			if info, _ := n.Tx(old.ID()); info.Status != StatusQueued ||
				info.DropReason != "" || info.DropRound != 0 {
				t.Fatalf("survivor %s = %+v, want untouched queued", old.ID(), info)
			} else {
				assertTxMatches(t, info.Tx, old)
			}
		}
		assertAccountQueue(t, n, keys[i].pub, 0, wantNotes)
	}

	// 一出一入，池仍恰好满；只动池，不推进轮次或确认高度。
	if got := n.st.poolSize(); got != uint64(len(pool)) {
		t.Fatalf("pool size after eviction = %d, want %d", got, len(pool))
	}
	if n.CurrentRound() != 1 {
		t.Fatalf("round = %d, want 1", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, want 0", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("eviction must not leave a confirmed block")
	}
}

// assertPoolFullBusiness 在一次竞争失败后核对：pool-full、无成功结果、被拒交易不留
// 历史；池内每笔交易与账户待处理列表保持操作前结果；池仍满；轮次与确认高度不变。
func assertPoolFullBusiness(t *testing.T, n *Node, keys []testKey,
	pool []*Transaction, rejected *Transaction, res *SubmitResult, err error,
) {
	t.Helper()
	if reason(err) != ReasonPoolFull {
		t.Fatalf("submit got %v, want %s", err, ReasonPoolFull)
	}
	if res != nil {
		t.Fatalf("rejected submit returned result %+v, want nil", res)
	}
	if _, qerr := n.Tx(rejected.ID()); reason(qerr) != ReasonUnknownTx {
		t.Fatalf("rejected tx query = %v, want %s", qerr, ReasonUnknownTx)
	}
	for i, old := range pool {
		info, qerr := n.Tx(old.ID())
		if qerr != nil {
			t.Fatalf("pool tx %s vanished: %v", old.ID(), qerr)
		}
		if info.Status != StatusQueued || info.DropReason != "" || info.DropRound != 0 {
			t.Fatalf("pool tx %s = %+v, want untouched queued", old.ID(), info)
		}
		assertTxMatches(t, info.Tx, old)
		assertAccountQueue(t, n, keys[i].pub, 0, map[string]string{
			old.ID(): "waiting-pack",
		})
	}
	// 被拒交易的账户此前为空，拒绝后仍为空。
	assertAccountQueue(t, n, keys[len(pool)].pub, 0, map[string]string{})
	if got := n.st.poolSize(); got != uint64(len(pool)) {
		t.Fatalf("pool size after rejection = %d, want still %d", got, len(pool))
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("rejection moved state: round=%d height=%d", n.CurrentRound(), n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("rejection must not leave a confirmed block")
	}
}

// 满池场景一：池内同时存在费用 0、2^63-1、2^63、2^64-1 的四笔排队交易，
// 新交易费用只需与其中优先级最低者（0 费用那笔）比较。
// 费用 1 的新交易严格高于 0：必须挤出 0 费用交易，而三笔高费用交易（包括跨过
// 2^63 的两笔和最大费用交易）一律保留。这同时证伪了有符号比较——若把 2^63/2^64-1
// 当成负数，被选中的淘汰对象会变成它们而不是 0 费用交易。
func TestFullPoolEvictsZeroFeeAmongExtremeFees(t *testing.T) {
	fees := []uint64{feeZero, feeIntMax, feeHighBit, feeUintMax}
	n, keys, pool := newFullQueuedPool(t, fees,
		[]string{"queued-zero", "queued-int-max", "queued-high-bit", "queued-uint-max"})
	victim := pool[0]

	win := NewTransaction(keys[len(pool)].priv, 1, []byte("win-fee-one"), 1, 100)
	res, err := n.Submit(win)
	if err != nil {
		t.Fatalf("fee 1 must displace the zero-fee tx: %v", err)
	}
	assertEvictionBusiness(t, n, keys, pool, victim, win, res)
}

// 满池场景二：明确钉住跨过 2^63 的高低关系。池中可淘汰对象费用为 2^63-1，
// 另有一笔费用 2^64-1 的交易。费用 2^63 的新交易严格高于 2^63-1，必须挤出
// 2^63-1 那笔；费用更高的 2^64-1 交易不得被误淘汰。
func TestFullPoolEvictsAcrossSignBit(t *testing.T) {
	fees := []uint64{feeIntMax, feeUintMax}
	n, keys, pool := newFullQueuedPool(t, fees,
		[]string{"queued-int-max", "queued-uint-max"})
	victim := pool[0] // 2^63-1：可淘汰对象中优先级最低者
	if pool[1].Fee != feeUintMax {
		t.Fatal("setup: second tx must carry the max fee and survive")
	}

	win := NewTransaction(keys[len(pool)].priv, 1, []byte("win-high-bit"), feeHighBit, 100)
	res, err := n.Submit(win)
	if err != nil {
		t.Fatalf("fee 2^63 must beat fee 2^63-1: %v", err)
	}
	assertEvictionBusiness(t, n, keys, pool, victim, win, res)
}

// 满池场景三：明确钉住最大费用与 0 之间的差距。池中最低费用为 0，另有一笔
// 2^63 交易；费用 2^64-1 的新交易挤出 0 费用交易，2^63 交易原样保留。
func TestFullPoolMaxFeeEvictsZeroFee(t *testing.T) {
	fees := []uint64{feeZero, feeHighBit}
	n, keys, pool := newFullQueuedPool(t, fees,
		[]string{"queued-zero", "queued-high-bit"})
	victim := pool[0]

	win := NewTransaction(keys[len(pool)].priv, 1, []byte("win-uint-max"), feeUintMax, 100)
	res, err := n.Submit(win)
	if err != nil {
		t.Fatalf("max-fee tx must displace the zero-fee tx: %v", err)
	}
	assertEvictionBusiness(t, n, keys, pool, victim, win, res)
}

// 满池场景四：0 费用的新交易也是合法竞争者。它与池中唯一可淘汰的 0 费用交易
// 同费，只能靠标识字典序更小进入：构造标识更小的 0 费用新交易，必须挤出那笔
// 0 费用交易，而不是因为费用为 0 被忽略、或反过来误淘汰更高费用的 2^63 交易。
func TestFullPoolZeroFeeTxWinsTieBySmallerID(t *testing.T) {
	fees := []uint64{feeZero, feeHighBit}
	n, keys, pool := newFullQueuedPool(t, fees,
		[]string{"queued-zero", "queued-high-bit"})
	victim := pool[0]

	smaller := craftTxWithID(t, keys[len(pool)].priv, 1, feeZero, victim.ID(), true)
	if smaller.ID() >= victim.ID() {
		t.Fatal("setup: new zero-fee tx id must be smaller than victim")
	}
	res, err := n.Submit(smaller)
	if err != nil {
		t.Fatalf("same-fee smaller-id zero-fee tx must evict: %v", err)
	}
	assertEvictionBusiness(t, n, keys, pool, victim, smaller, res)
}

// 同一场景的反面：0 费用新交易标识更大时不能挤出同费的池中交易，必须 pool-full，
// 池状态与账户队列完全不变。
func TestFullPoolZeroFeeTxLosesTieByLargerID(t *testing.T) {
	fees := []uint64{feeZero, feeHighBit}
	n, keys, pool := newFullQueuedPool(t, fees,
		[]string{"queued-zero", "queued-high-bit"})

	larger := craftTxWithID(t, keys[len(pool)].priv, 1, feeZero, pool[0].ID(), false)
	if larger.ID() <= pool[0].ID() {
		t.Fatal("setup: new zero-fee tx id must be larger than victim")
	}
	res, err := n.Submit(larger)
	assertPoolFullBusiness(t, n, keys, pool, larger, res, err)
}

// 费用取无符号最大值时的同费标识规则：不存在比最大费用更大的合法值，此时能否
// 接收只能由同费标识关系决定。池中两笔费用均为 2^64-1 的排队交易，标识较大者是
// 唯一可淘汰对象。
func TestFullPoolMaxFeeTieBreakByTxID(t *testing.T) {
	// 标识更小的最大费用新交易：挤出池中标识最大者，标识更小的池内交易幸存。
	t.Run("smaller-id admitted", func(t *testing.T) {
		fees := []uint64{feeUintMax, feeUintMax}
		n, keys, pool := newFullQueuedPool(t, fees,
			[]string{"queued-max-a", "queued-max-b"})
		bigID := pool[0].ID()
		if pool[1].ID() > bigID {
			bigID = pool[1].ID()
		}
		var victim *Transaction
		for _, old := range pool {
			if old.ID() == bigID {
				victim = old
			}
		}

		smaller := craftTxWithID(t, keys[len(pool)].priv, 1, feeUintMax, bigID, true)
		if smaller.ID() >= bigID {
			t.Fatalf("setup: crafted id %s must be smaller than victim %s", smaller.ID(), bigID)
		}
		res, err := n.Submit(smaller)
		if err != nil {
			t.Fatalf("same-max-fee smaller-id tx must evict: %v", err)
		}
		assertEvictionBusiness(t, n, keys, pool, victim, smaller, res)
	})

	// 标识更大的最大费用新交易无处可胜（没有更高费用可言），必须 pool-full。
	t.Run("larger-id rejected", func(t *testing.T) {
		fees := []uint64{feeUintMax, feeUintMax}
		n, keys, pool := newFullQueuedPool(t, fees,
			[]string{"queued-max-a", "queued-max-b"})
		bigID := pool[0].ID()
		if pool[1].ID() > bigID {
			bigID = pool[1].ID()
		}

		larger := craftTxWithID(t, keys[len(pool)].priv, 1, feeUintMax, bigID, false)
		if larger.ID() <= bigID {
			t.Fatalf("setup: crafted id %s must be larger than %s", larger.ID(), bigID)
		}
		res, err := n.Submit(larger)
		assertPoolFullBusiness(t, n, keys, pool, larger, res, err)
	})
}

// 新交易费用更低时，即使标识更小也不能抢占位置：池中最低费用为 1，构造一笔
// 标识比它更小的 0 费用新交易，仍必须 pool-full，费用 1 与更高费用的交易都保留。
func TestFullPoolLowerFeeRejectedEvenWithSmallerID(t *testing.T) {
	fees := []uint64{1, feeHighBit}
	n, keys, pool := newFullQueuedPool(t, fees,
		[]string{"queued-one", "queued-high-bit"})

	cheapSmall := craftTxWithID(t, keys[len(pool)].priv, 1, feeZero, pool[0].ID(), true)
	if cheapSmall.ID() >= pool[0].ID() {
		t.Fatal("setup: cheaper tx id must be smaller than the fee-1 victim")
	}
	res, err := n.Submit(cheapSmall)
	assertPoolFullBusiness(t, n, keys, pool, cheapSmall, res, err)
}
