package consensus

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"
)

func newCapNode(t *testing.T, keys []testKey, maxTxs, cap uint64) (*Node, string) {
	t.Helper()
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("cap-seed"), Validators: vals, MaxTxsPerBlock: maxTxs, PoolCapacity: cap})
	if err != nil {
		t.Fatal(err)
	}
	return n, dir
}

// 容量配置可查询；0 与省略均为不限制。
func TestCapacityConfig(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 3)
	if c := n.Config().PoolCapacity; c != 3 {
		t.Fatalf("PoolCapacity = %d, want 3", c)
	}
	// 省略 PoolCapacity 为 0（不限制）。
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n2, err := New(dir, Config{Seed: []byte("s"), Validators: vals, MaxTxsPerBlock: 10})
	if err != nil {
		t.Fatal(err)
	}
	if c := n2.Config().PoolCapacity; c != 0 {
		t.Fatalf("omitted PoolCapacity = %d, want 0 (unlimited)", c)
	}
}

// 池满时费用更高者挤出最低费用交易；被挤出交易状态完整、带原因与轮次。
func TestCapacityEvictionByFee(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	if _, err := n.Submit(a); err != nil {
		t.Fatal(err)
	}
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 5, 100)
	res, err := n.Submit(b)
	if err != nil {
		t.Fatalf("higher fee should evict: %v", err)
	}
	if res.EvictedID != a.ID() {
		t.Fatalf("evicted = %s, want %s", res.EvictedID, a.ID())
	}
	// 新交易正常接收。
	bi, err := n.Tx(b.ID())
	if err != nil || bi.Status != StatusQueued {
		t.Fatalf("new tx status = %v %v", bi, err)
	}
	// 被挤出交易保留完整内容，状态 dropped，带原因与发生轮次，无确认块/替换关联。
	ai, err := n.Tx(a.ID())
	if err != nil {
		t.Fatal(err)
	}
	if ai.Status != StatusDropped {
		t.Fatalf("dropped status = %s, want dropped", ai.Status)
	}
	if ai.DropReason != DropReasonPoolCapacity || ai.DropRound != 1 {
		t.Fatalf("drop reason/round = %s/%d, want pool-capacity/1", ai.DropReason, ai.DropRound)
	}
	if ai.Tx == nil || string(ai.Tx.Content) != "a" {
		t.Fatalf("dropped tx content not preserved: %+v", ai.Tx)
	}
	if ai.BlockHeight != 0 || ai.BlockID != "" || ai.ReplacedBy != "" {
		t.Fatalf("dropped tx must have no block/replaced association: %+v", ai)
	}
}

// 池满且新交易不占优时拒绝 pool-full，不留下历史。
func TestCapacityPoolFullReject(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 5, 100)
	n.Submit(a)
	// 费用更低：拒绝。
	lower := NewTransaction(keys[1].priv, 1, []byte("low"), 1, 100)
	if _, err := n.Submit(lower); reason(err) != ReasonPoolFull {
		t.Fatalf("lower fee got %v, want %s", err, ReasonPoolFull)
	}
	// 费用相同但标识更大：拒绝。构造一笔同费且标识大于 a 的交易。
	var eq *Transaction
	for i := 0; ; i++ {
		cand := NewTransaction(keys[1].priv, 1, []byte(fmt.Sprintf("eq%d", i)), 5, 100)
		if cand.ID() > a.ID() {
			eq = cand
			break
		}
	}
	if _, err := n.Submit(eq); reason(err) != ReasonPoolFull {
		t.Fatalf("equal fee larger id got %v, want %s", err, ReasonPoolFull)
	}
	// 被拒交易不留下历史。
	if _, err := n.Tx(lower.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx should be unknown, got %v", err)
	}
	if _, err := n.Tx(eq.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx should be unknown, got %v", err)
	}
	// 腾出位置后可重新提交：a 被确认后位置释放。
	p, _ := n.Propose()
	confirmByVotes(t, n, keys, p.BlockID)
	if _, err := n.Submit(lower); err != nil {
		t.Fatalf("resubmit after space freed should succeed: %v", err)
	}
}

// 费用相同时挤出标识字典序最大者；新交易标识更小时才能挤出。
func TestCapacityEvictionTieBreak(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 2)

	// 两笔排队交易，费用相同。用不同发送者/内容制造标识差异。
	var txs []*Transaction
	for i := 0; i < 2; i++ {
		txs = append(txs, NewTransaction(keys[i].priv, 1, []byte(fmt.Sprintf("tx%d", i)), 5, 100))
		n.Submit(txs[i])
	}
	// 可淘汰者是标识字典序最大的那笔。
	victim := txs[0]
	for _, tx := range txs[1:] {
		if tx.ID() > victim.ID() {
			victim = tx
		}
	}
	// 构造一笔费用相同、标识比 victim 更小的新交易 → 可挤出。
	var newTx *Transaction
	for i := 0; ; i++ {
		cand := NewTransaction(keys[2].priv, 1, []byte(fmt.Sprintf("new%d", i)), 5, 100)
		if cand.ID() < victim.ID() {
			newTx = cand
			break
		}
	}
	res, err := n.Submit(newTx)
	if err != nil {
		t.Fatalf("equal fee smaller id should evict: %v", err)
	}
	if res.EvictedID != victim.ID() {
		t.Fatalf("evicted = %s, want %s (lex largest)", res.EvictedID, victim.ID())
	}
}

// 被未决候选引用的交易受保护：池满时无可淘汰者，拒绝 pool-full。
func TestCapacityProtectedByProposal(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)
	n.Propose() // a 被本地提议引用，受保护。
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 99, 100)
	if _, err := n.Submit(b); reason(err) != ReasonPoolFull {
		t.Fatalf("proposal-protected tx got %v, want %s", err, ReasonPoolFull)
	}
	// a 仍是 proposed，b 未入池。
	if info, _ := n.Tx(a.ID()); info.Status != StatusProposed {
		t.Fatalf("a should remain proposed: %+v", info)
	}
	if _, err := n.Tx(b.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("b should be unknown: %v", err)
	}
}

// 合法加费替换不增加计数，满池也可执行，且不淘汰别的交易。
func TestCapacityFeeReplaceNoEviction(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)
	// 满池时同发送者同序号加费替换：可执行，不淘汰。
	hi := NewTransaction(keys[0].priv, 1, []byte("a2"), 5, 100)
	res, err := n.Submit(hi)
	if err != nil {
		t.Fatalf("fee replacement should succeed even when full: %v", err)
	}
	if res.EvictedID != "" {
		t.Fatalf("replacement must not evict, got %s", res.EvictedID)
	}
	if res.ReplacedID != a.ID() {
		t.Fatalf("replaced = %s, want %s", res.ReplacedID, a.ID())
	}
	// 池内仍只有一笔（hi），状态 queued。
	if info, _ := n.Tx(hi.ID()); info.Status != StatusQueued {
		t.Fatalf("hi status = %s, want queued", info.Status)
	}
	// 被提议引用的交易禁止替换（即使满池）。
	n2, _ := newCapNode(t, keys, 10, 1)
	x := NewTransaction(keys[0].priv, 1, []byte("x"), 1, 100)
	n2.Submit(x)
	n2.Propose()
	xhi := NewTransaction(keys[0].priv, 1, []byte("x2"), 9, 100)
	if _, err := n2.Submit(xhi); reason(err) != ReasonProposalLocked {
		t.Fatalf("proposal-locked replacement got %v, want %s", err, ReasonProposalLocked)
	}
}

// 被挤出交易不能再用于登记候选；完全相同旧交易按重复拒绝；同序号新交易可竞争。
func TestCapacityDroppedTxRules(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 5, 100)
	n.Submit(b) // a 被挤出。

	// 被挤出交易不能登记为候选。
	n.Propose()
	if _, err := n.RegisterCandidate(1, []string{a.ID()}); reason(err) != ReasonTxNotInPool {
		t.Fatalf("dropped tx register got %v, want %s", err, ReasonTxNotInPool)
	}
	// 完全相同的旧交易再次提交按重复拒绝。
	if _, err := n.Submit(NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)); reason(err) != ReasonDuplicate {
		t.Fatalf("exact resubmit got %v, want %s", err, ReasonDuplicate)
	}
	// 同发送者同序号的新交易可按普通规则竞争位置：在另一节点中 b 仍排队（未被提议保护），
	// 费用更高的同序号新交易可将其挤出。
	n2, _ := newCapNode(t, keys, 10, 1)
	n2.Submit(NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100))
	b = NewTransaction(keys[1].priv, 1, []byte("b"), 5, 100)
	n2.Submit(b) // a 被挤出，b 排队。
	c := NewTransaction(keys[0].priv, 1, []byte("c"), 9, 100)
	res, err := n2.Submit(c)
	if err != nil {
		t.Fatalf("new same-seq tx should compete: %v", err)
	}
	if res.EvictedID != b.ID() {
		t.Fatalf("new tx should evict current occupant b, got %s", res.EvictedID)
	}
}

// 淘汰不推进确认序号；留下缺口时账户查询指出最早缺口，后续序号保留，不能越缺口打包。
func TestCapacityEvictionGap(t *testing.T) {
	keys := genKeys(t, 4)
	// 容量 2：账户0 的 seq1（低费）与 seq2 占满两格。
	n, _ := newCapNode(t, keys, 10, 2)
	tx1 := NewTransaction(keys[0].priv, 1, []byte("1"), 1, 100)
	tx2 := NewTransaction(keys[0].priv, 2, []byte("2"), 5, 100)
	n.Submit(tx1)
	n.Submit(tx2)
	// 更高费新交易挤入：可淘汰者中 tx1 费用最低（1），被挤出。
	hi := NewTransaction(keys[2].priv, 1, []byte("hi"), 9, 100)
	res, err := n.Submit(hi)
	if err != nil {
		t.Fatal(err)
	}
	if res.EvictedID != tx1.ID() {
		t.Fatalf("evicted = %s, want tx1 (lowest fee)", res.EvictedID)
	}
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 {
		t.Fatalf("confirmed seq = %d, want 0 (eviction does not advance)", acct.ConfirmedSequence)
	}
	// seq1 被挤出，seq2 仍保留 → 最早缺口为 1。
	if acct.Gap != 1 {
		t.Fatalf("gap = %d, want 1", acct.Gap)
	}
	if len(acct.Pending) != 1 || acct.Pending[0].ID != tx2.ID() {
		t.Fatalf("pending = %+v, want only tx2", acct.Pending)
	}
	// 不能越过缺口打包：本地提议不含 tx2。
	p, _ := n.Propose()
	for _, id := range p.TxIDs {
		if id == tx2.ID() {
			t.Fatal("must not pack past gap")
		}
	}
}

// 确认释放位置；落选回池交易仍占位置；共享交易不重复计数。
func TestCapacitySlotLifecycle(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 2)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 1, 100)
	n.Submit(a)
	n.Submit(b)
	local, _ := n.Propose() // a、b 均被引用（上限10）。
	// 两笔都受保护，新交易无法挤入。
	c := NewTransaction(keys[2].priv, 1, []byte("c"), 9, 100)
	if _, err := n.Submit(c); reason(err) != ReasonPoolFull {
		t.Fatalf("all protected got %v, want %s", err, ReasonPoolFull)
	}
	// 确认本地提议：a、b 释放位置。
	confirmByVotes(t, n, keys, local.BlockID)
	// 现在可接收新交易。
	if _, err := n.Submit(c); err != nil {
		t.Fatalf("slot should free after confirm: %v", err)
	}
}

// dropped 历史不能随后变成 expired。
func TestCapacityDroppedNotExpired(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 1)

	// 到期轮次很靠后的交易被挤出。
	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 5, 100)
	n.Submit(b) // a 被挤出，drop_round=1。
	// 结束多轮让当前轮次超过 a 的到期轮次。
	for i := 0; i < 5; i++ {
		n.EndRound()
	}
	ai, _ := n.Tx(a.ID())
	if ai.Status != StatusDropped {
		t.Fatalf("dropped tx must not become expired, got %s", ai.Status)
	}
}

// 重启恢复容量、淘汰记录与候选保护。
func TestCapacityPersistence(t *testing.T) {
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("cap-persist"), Validators: vals, MaxTxsPerBlock: 10, PoolCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 5, 100)
	n.Submit(b) // a 被挤出。
	// 提议引用 b，使其受保护。
	n.Propose()

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n2.Config().PoolCapacity != 1 {
		t.Fatalf("restored capacity = %d, want 1", n2.Config().PoolCapacity)
	}
	ai, err := n2.Tx(a.ID())
	if err != nil || ai.Status != StatusDropped || ai.DropReason != DropReasonPoolCapacity || ai.DropRound != 1 {
		t.Fatalf("dropped record not restored: %+v %v", ai, err)
	}
	// 恢复后候选保护仍生效：b 被未决候选引用，池满时新交易无法挤入。
	c := NewTransaction(keys[2].priv, 1, []byte("c"), 9, 100)
	if _, err := n2.Submit(c); reason(err) != ReasonPoolFull {
		t.Fatalf("restored pool should be full (b protected), got %v", err)
	}
}

// 旧版本状态未保存容量时按不限制处理。
func TestCapacityOldStateUnlimited(t *testing.T) {
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("old"), Validators: vals, MaxTxsPerBlock: 10})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		n.Submit(NewTransaction(keys[i%len(keys)].priv, uint64(i+1), []byte(fmt.Sprintf("t%d", i)), 1, 100))
	}
	// 模拟旧版本：删除 pool_capacity 字段。
	st := n.st.clone()
	st.PoolCap = 0
	writeRawState(t, dir, st)

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n2.Config().PoolCapacity != 0 {
		t.Fatalf("old state capacity = %d, want 0 (unlimited)", n2.Config().PoolCapacity)
	}
	// 不限制：仍可继续接收。
	if _, err := n2.Submit(NewTransaction(keys[0].priv, 10, []byte("more"), 1, 100)); err != nil {
		t.Fatalf("unlimited pool should accept: %v", err)
	}
}

// 保存失败时接收与淘汰都不生效：两笔交易状态与账户查询保持操作前结果。
func TestCapacityAtomicity(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newCapNode(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)

	n.injectSaveErr = errors.New("disk full (simulated)")
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 5, 100)
	_, err := n.Submit(b)
	if err == nil {
		t.Fatal("expected save error")
	}
	n.injectSaveErr = nil

	// 操作前状态：a 仍 queued，b 未知。
	if info, _ := n.Tx(a.ID()); info.Status != StatusQueued {
		t.Fatalf("a status = %s, want queued (pre-op)", info.Status)
	}
	if _, err := n.Tx(b.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("b should be unknown after failed save, got %v", err)
	}
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || len(acct.Pending) != 1 {
		t.Fatalf("account should be pre-op: %+v", acct)
	}
}

// 确定性：相同初始配置与输入顺序产生相同接收决定、淘汰对象与后续确认结果。
func TestCapacityDeterminism(t *testing.T) {
	keys := genKeys(t, 4)
	build := func() []string {
		dir := t.TempDir()
		vals := make([][]byte, len(keys))
		for i, k := range keys {
			vals[i] = append([]byte(nil), k.pub...)
		}
		n, err := New(dir, Config{Seed: []byte("cap-det"), Validators: vals, MaxTxsPerBlock: 3, PoolCapacity: 2})
		if err != nil {
			t.Fatal(err)
		}
		var dropped []string
		// 固定输入顺序：费用递增，制造淘汰。
		for i := 0; i < 6; i++ {
			tx := NewTransaction(keys[i%4].priv, uint64(i+1), []byte(fmt.Sprintf("t%d", i)), uint64(i+1), 100)
			res, err := n.Submit(tx)
			if err != nil {
				if reason(err) == ReasonPoolFull {
					continue
				}
				t.Fatal(err)
			}
			if res.EvictedID != "" {
				dropped = append(dropped, res.EvictedID)
			}
		}
		return dropped
	}
	d1 := build()
	d2 := build()
	if fmt.Sprint(d1) != fmt.Sprint(d2) {
		t.Fatalf("eviction decisions differ:\n%v\n%v", d1, d2)
	}
}

var _ = ed25519.PublicKeySize
