package consensus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newTestNodeCap(t *testing.T, keys []testKey, maxTxs, capacity uint64) (*Node, string) {
	t.Helper()
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("test-seed-cap"), Validators: vals, MaxTxsPerBlock: maxTxs, PoolCapacity: capacity})
	if err != nil {
		t.Fatal(err)
	}
	return n, dir
}

// 容量进入配置查询并随状态持久化；省略时为 0（不限制）。
func TestCapacityConfigAndQuery(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNodeCap(t, keys, 10, 3)
	cfg := n.Config()
	if cfg.PoolCapacity != 3 || cfg.MaxTxsPerBlock != 10 {
		t.Fatalf("config = %+v, want capacity 3 maxTxs 10", cfg)
	}
	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := n2.Config().PoolCapacity; got != 3 {
		t.Fatalf("restored capacity = %d, want 3", got)
	}

	// 省略容量：保持现有行为（不限制）。
	n3, _ := newTestNode(t, keys, 10)
	if got := n3.Config().PoolCapacity; got != 0 {
		t.Fatalf("default capacity = %d, want 0 (unlimited)", got)
	}
	for i := 0; i < 20; i++ {
		tx := NewTransaction(keys[0].priv, uint64(i+1), []byte(fmt.Sprintf("t%d", i)), 1, 1000)
		if _, err := n3.Submit(tx); err != nil {
			t.Fatalf("unlimited pool rejected tx %d: %v", i, err)
		}
	}
}

// 满池时费用不更高的新交易被 pool-full 拒绝：池不变、不留历史、腾出位置后可重新提交。
// 费用更高时挤出费用最低者，结果给出被挤出标识，旧交易保留完整内容并显示 dropped。
func TestCapacityEviction(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 2)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 5, 100)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 3, 100)
	for _, tx := range []*Transaction{a, b} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 费用低于池中最低：拒绝，池保持不变，且不留任何历史。
	cheap := NewTransaction(keys[2].priv, 1, []byte("cheap"), 2, 100)
	if _, err := n.Submit(cheap); reason(err) != ReasonPoolFull {
		t.Fatalf("cheap tx got %v, want %s", err, ReasonPoolFull)
	}
	if _, err := n.Tx(cheap.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx left history: %v", err)
	}
	if info, _ := n.Tx(a.ID()); info.Status != StatusQueued {
		t.Fatalf("rejected submit changed pool: a = %s", info.Status)
	}
	if info, _ := n.Tx(b.ID()); info.Status != StatusQueued {
		t.Fatalf("rejected submit changed pool: b = %s", info.Status)
	}

	// 费用更高：挤出费用最低的 b。
	d := NewTransaction(keys[2].priv, 1, []byte("d"), 4, 100)
	res, err := n.Submit(d)
	if err != nil {
		t.Fatal(err)
	}
	if res.EvictedID != b.ID() || res.TxID != d.ID() || res.ReplacedID != "" {
		t.Fatalf("eviction result wrong: %+v", res)
	}

	// 被挤出的交易保留完整内容，显示 dropped/pool-capacity/发生轮次，
	// 不带确认块或替换交易关联。
	info, err := n.Tx(b.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 1 {
		t.Fatalf("dropped tx wrong: %+v", info)
	}
	if info.BlockHeight != 0 || info.BlockID != "" || info.ReplacedBy != "" {
		t.Fatalf("dropped tx must not carry block/replace links: %+v", info)
	}
	if info.Tx == nil || string(info.Tx.Content) != "b" || info.Tx.Fee != 3 {
		t.Fatalf("dropped tx content not preserved: %+v", info.Tx)
	}
	// 不再占用位置：账户待处理列表为空。
	if acct := n.Account(keys[1].pub); len(acct.Pending) != 0 {
		t.Fatalf("dropped tx still pending: %+v", acct)
	}

	// 再次提交完全相同的旧交易仍按重复拒绝。
	if _, err := n.Submit(NewTransaction(keys[1].priv, 1, []byte("b"), 3, 100)); reason(err) != ReasonDuplicate {
		t.Fatalf("resubmit dropped tx got %v, want %s", err, ReasonDuplicate)
	}

	// 同发送者同序号的新交易按普通提交规则竞争位置：费用足够高即可挤出别人。
	b2 := NewTransaction(keys[1].priv, 1, []byte("b2"), 10, 100)
	res, err = n.Submit(b2)
	if err != nil {
		t.Fatal(err)
	}
	if res.EvictedID != d.ID() || res.ReplacedID != "" {
		t.Fatalf("fresh same-seq tx should compete normally: %+v", res)
	}

	// 被挤出的交易不能再用于登记候选。
	if _, err := n.Propose(); err != nil {
		t.Fatal(err)
	}
	if _, err := n.RegisterCandidate(1, []string{d.ID()}); reason(err) != ReasonTxNotInPool {
		t.Fatalf("dropped tx in candidate got %v, want %s", err, ReasonTxNotInPool)
	}
}

// 同费时淘汰标识字典序最大者；新交易费用相同且标识更小时才能挤出，否则 pool-full。
func TestCapacityEvictionTieBreak(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 2)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 5, 100)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 5, 100)
	n.Submit(a)
	n.Submit(b)
	victim := a.ID()
	if b.ID() > victim {
		victim = b.ID()
	}

	// 构造两笔同费交易：一笔标识小于淘汰对象（可挤出），一笔大于（被拒绝）。
	var smaller, larger *Transaction
	for i := 0; smaller == nil || larger == nil; i++ {
		tx := NewTransaction(keys[2].priv, uint64(i+1), []byte(fmt.Sprintf("c%d", i)), 5, 100)
		if tx.ID() < victim && smaller == nil {
			smaller = tx
		}
		if tx.ID() > victim && larger == nil {
			larger = tx
		}
	}

	res, err := n.Submit(smaller)
	if err != nil {
		t.Fatalf("same-fee smaller-id tx should evict: %v", err)
	}
	if res.EvictedID != victim {
		t.Fatalf("evicted %s, want lexicographically largest %s", res.EvictedID, victim)
	}

	// 池中现为幸存者与新交易（同费）：同费但标识更大的交易不能挤出。
	curVictim := a.ID()
	if curVictim == victim {
		curVictim = b.ID()
	}
	if smaller.ID() > curVictim {
		curVictim = smaller.ID()
	}
	if larger.ID() < curVictim {
		t.Fatal("test setup: larger tx id must exceed current victim")
	}
	if _, err := n.Submit(larger); reason(err) != ReasonPoolFull {
		t.Fatalf("same-fee larger-id tx got %v, want %s", err, ReasonPoolFull)
	}
	if _, err := n.Tx(larger.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx left history: %v", err)
	}
}

// 满池时合法的加费替换不增加计数、可执行且不淘汰其他交易；被未决候选引用的交易仍禁止替换。
func TestCapacityReplacementWhenFull(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	if _, err := n.Submit(a); err != nil {
		t.Fatal(err)
	}
	// 满池替换：不淘汰任何交易。
	ahi := NewTransaction(keys[0].priv, 1, []byte("ahi"), 2, 100)
	res, err := n.Submit(ahi)
	if err != nil {
		t.Fatalf("replacement on full pool must succeed: %v", err)
	}
	if res.ReplacedID != a.ID() || res.EvictedID != "" {
		t.Fatalf("replacement must not evict: %+v", res)
	}
	// 计数不变：池仍满，新交易仍需竞争。
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 1, 100)
	if _, err := n.Submit(b); reason(err) != ReasonPoolFull {
		t.Fatalf("got %v, want %s", err, ReasonPoolFull)
	}

	// 被未决候选引用的交易仍禁止替换。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TxIDs) != 1 || p.TxIDs[0] != ahi.ID() {
		t.Fatalf("proposal = %v, want [ahi]", p.TxIDs)
	}
	locked := NewTransaction(keys[0].priv, 1, []byte("locked"), 99, 100)
	if _, err := n.Submit(locked); reason(err) != ReasonProposalLocked {
		t.Fatalf("replacing proposed tx got %v, want %s", err, ReasonProposalLocked)
	}
}

// 被未决候选引用的交易受保护：占位但不可淘汰；轮次结束后只解除保护、仍占位置。
func TestCapacityProtection(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)
	if _, err := n.Propose(); err != nil { // a 进入本地提议，受保护
		t.Fatal(err)
	}

	// 池中唯一交易受保护：任何费用的全新交易都无可淘汰者。
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 1000, 100)
	if _, err := n.Submit(b); reason(err) != ReasonPoolFull {
		t.Fatalf("got %v, want %s", err, ReasonPoolFull)
	}

	// 主动结束轮次：a 回到排队（未到期，仍占位置），保护解除。
	if _, err := n.EndRound(); err != nil {
		t.Fatal(err)
	}
	if info, _ := n.Tx(a.ID()); info.Status != StatusQueued {
		t.Fatalf("a after EndRound = %s, want queued", info.Status)
	}
	res, err := n.Submit(b)
	if err != nil {
		t.Fatalf("unprotected tx should be evictable: %v", err)
	}
	if res.EvictedID != a.ID() {
		t.Fatalf("evicted %s, want %s", res.EvictedID, a.ID())
	}
	if info, _ := n.Tx(a.ID()); info.Status != StatusDropped || info.DropRound != 2 {
		t.Fatalf("a should be dropped in round 2: %+v", info)
	}
}

// 共享交易只计一笔：多候选引用不重复计数；胜出确认后释放位置。
func TestCapacitySharedTxCountsOnce(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 2)

	shared := NewTransaction(keys[0].priv, 1, []byte("shared"), 1, 100)
	other := NewTransaction(keys[1].priv, 1, []byte("other"), 1, 100)
	n.Submit(shared)
	n.Submit(other)
	local, _ := n.Propose() // 本地提议含两笔
	if len(local.TxIDs) != 2 {
		t.Fatalf("local proposal = %v, want both txs", local.TxIDs)
	}
	// 竞争候选只含 shared：shared 被两个候选引用，仍只算一笔（池计数 2，已满）。
	if _, err := n.RegisterCandidate(1, []string{shared.ID()}); err != nil {
		t.Fatal(err)
	}
	c := NewTransaction(keys[2].priv, 1, []byte("c"), 1000, 100)
	if _, err := n.Submit(c); reason(err) != ReasonPoolFull {
		t.Fatalf("protected pool got %v, want %s", err, ReasonPoolFull)
	}

	// 确认本地提议：两笔交易确认，位置全部释放。
	confirmByVotes(t, n, keys, local.BlockID)
	if _, err := n.Submit(c); err != nil {
		t.Fatalf("confirmation must free slots: %v", err)
	}
	d := NewTransaction(keys[3].priv, 1, []byte("d"), 1, 100)
	if _, err := n.Submit(d); err != nil {
		t.Fatalf("second freed slot: %v", err)
	}
}

// 已确认、被替换、过期与被挤出的历史均不占位置。
func TestCapacityHistoryDoesNotCount(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 1)

	// 确认释放位置。
	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)
	p, _ := n.Propose()
	confirmByVotes(t, n, keys, p.BlockID)

	// 被替换不占位置：替换后计数仍为 1。
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 1, 100)
	n.Submit(b)
	bhi := NewTransaction(keys[1].priv, 1, []byte("bhi"), 2, 100)
	if _, err := n.Submit(bhi); err != nil {
		t.Fatalf("replacement must not grow count: %v", err)
	}
	c := NewTransaction(keys[2].priv, 1, []byte("c"), 5, 3) // 进入轮次 3 时到期
	res, err := n.Submit(c)                                 // 池满：挤出 bhi
	if err != nil || res.EvictedID != bhi.ID() {
		t.Fatalf("eviction after replacement: %+v %v", res, err)
	}

	// 过期释放位置：c 在进入轮次 3 时失效。
	d := NewTransaction(keys[3].priv, 1, []byte("d"), 1, 100)
	if _, err := n.Submit(d); reason(err) != ReasonPoolFull {
		t.Fatalf("full pool got %v, want %s", err, ReasonPoolFull)
	}
	if _, err := n.EndRound(); err != nil { // 轮次 2
		t.Fatal(err)
	}
	if _, err := n.EndRound(); err != nil { // 轮次 3：c 到期
		t.Fatal(err)
	}
	if info, _ := n.Tx(c.ID()); info.Status != StatusExpired {
		t.Fatalf("c = %s, want expired", info.Status)
	}
	if _, err := n.Submit(d); err != nil {
		t.Fatalf("expiry must free slot: %v", err)
	}
}

// 淘汰不推进已确认序号；留下缺口时账户查询指出最早缺口，后续序号保留但不能越过缺口打包。
func TestCapacityEvictionLeavesGap(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 2)

	s1 := NewTransaction(keys[0].priv, 1, []byte("s1"), 1, 100)
	s2 := NewTransaction(keys[0].priv, 2, []byte("s2"), 2, 100)
	n.Submit(s1)
	n.Submit(s2)

	x := NewTransaction(keys[1].priv, 1, []byte("x"), 5, 100)
	res, err := n.Submit(x) // 挤出费用最低的 s1，留下序号缺口
	if err != nil || res.EvictedID != s1.ID() {
		t.Fatalf("eviction: %+v %v", res, err)
	}
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 1 || len(acct.Pending) != 1 || acct.Pending[0].ID != s2.ID() {
		t.Fatalf("account after eviction wrong: %+v", acct)
	}

	// s2 不能越过缺口被打包。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TxIDs) != 1 || p.TxIDs[0] != x.ID() {
		t.Fatalf("proposal = %v, want only x (s2 blocked by gap)", p.TxIDs)
	}
	confirmByVotes(t, n, keys, p.BlockID)
	acct = n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 1 || len(acct.Pending) != 1 {
		t.Fatalf("gap must survive unrelated confirmation: %+v", acct)
	}
}

// 接收新交易与淘汰旧交易必须一起成功：保存失败时两笔交易状态与账户查询保持操作前结果。
func TestCapacityEvictionAtomicity(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNodeCap(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)
	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	n.injectSaveErr = errors.New("disk full (simulated)")
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 2, 100)
	res, err := n.Submit(b)
	if err == nil {
		t.Fatal("submit must fail when save fails")
	}
	if res != nil {
		t.Fatalf("result should be nil on error, got %+v", res)
	}
	n.injectSaveErr = nil

	// 两笔交易都保持操作前状态：a 仍排队，b 无记录。
	if info, _ := n.Tx(a.ID()); info.Status != StatusQueued {
		t.Fatalf("a = %s, want queued after failed eviction", info.Status)
	}
	if _, err := n.Tx(b.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("b should be unknown after failed submit: %v", err)
	}
	if acct := n.Account(keys[0].pub); len(acct.Pending) != 1 || acct.Pending[0].ID != a.ID() {
		t.Fatalf("account changed despite failed save: %+v", acct)
	}
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("state file changed despite failed write")
	}

	// 恢复写入后同一提交正常完成。
	res, err = n.Submit(b)
	if err != nil || res.EvictedID != a.ID() {
		t.Fatalf("retry after recovery: %+v %v", res, err)
	}
}

// 重启后恢复容量、淘汰记录与候选保护；版本 1、2 未保存容量时按不限制处理。
func TestCapacityPersistence(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNodeCap(t, keys, 10, 2)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 2, 100)
	n.Submit(a)
	n.Submit(b)
	c := NewTransaction(keys[2].priv, 1, []byte("c"), 3, 100)
	res, err := n.Submit(c) // 挤出 a
	if err != nil || res.EvictedID != a.ID() {
		t.Fatalf("eviction: %+v %v", res, err)
	}
	p, _ := n.Propose() // b、c 进入提议，受保护

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := n2.Config().PoolCapacity; got != 2 {
		t.Fatalf("restored capacity = %d, want 2", got)
	}
	// 淘汰记录恢复。
	info, err := n2.Tx(a.ID())
	if err != nil || info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 1 {
		t.Fatalf("dropped record not restored: %+v %v", info, err)
	}
	// 候选保护恢复：仍禁止替换、仍不可淘汰。
	locked := NewTransaction(keys[1].priv, 1, []byte("locked"), 99, 100)
	if _, err := n2.Submit(locked); reason(err) != ReasonProposalLocked {
		t.Fatalf("replacing protected tx after reopen got %v, want %s", err, ReasonProposalLocked)
	}
	d := NewTransaction(keys[3].priv, 1, []byte("d"), 1000, 100)
	if _, err := n2.Submit(d); reason(err) != ReasonPoolFull {
		t.Fatalf("protected pool after reopen got %v, want %s", err, ReasonPoolFull)
	}
	// 确认后位置释放，可正常接收。
	confirmByVotes(t, n2, keys, p.BlockID)
	if _, err := n2.Submit(d); err != nil {
		t.Fatalf("slots must be freed after confirm: %v", err)
	}
}

// 版本 2 的状态文件没有容量字段：打开后按不限制处理，确认历史保持原样。
func TestOpenV2StateWithoutCapacity(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNodeCap(t, keys, 10, 7)
	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(a)
	p, _ := n.Propose()
	confirmByVotes(t, n, keys, p.BlockID)

	// 改写为版本 2 并删除容量字段。
	raw, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["version"] = float64(2)
	delete(doc, "pool_capacity")
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), out, 0o644); err != nil {
		t.Fatal(err)
	}

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := n2.Config().PoolCapacity; got != 0 {
		t.Fatalf("v2 state capacity = %d, want 0 (unlimited)", got)
	}
	if n2.Height() != 1 || n2.CurrentRound() != 2 {
		t.Fatalf("history changed: height=%d round=%d", n2.Height(), n2.CurrentRound())
	}
	if info, _ := n2.Tx(a.ID()); info.Status != StatusConfirmed || info.BlockHeight != 1 {
		t.Fatalf("confirmed history not preserved: %+v", info)
	}
	// 不限制：可连续接收多笔交易。
	for i := 0; i < 10; i++ {
		tx := NewTransaction(keys[1].priv, uint64(i+1), []byte(fmt.Sprintf("t%d", i)), 1, 1000)
		if _, err := n2.Submit(tx); err != nil {
			t.Fatalf("v2 state should be unlimited: %v", err)
		}
	}
}

// 确定性：相同初始配置与输入顺序产生相同的接收决定、淘汰对象与后续确认结果。
func TestCapacityDeterminism(t *testing.T) {
	keys := genKeys(t, 4)
	run := func() ([]string, []string) {
		vals := make([][]byte, len(keys))
		for i, k := range keys {
			vals[i] = append([]byte(nil), k.pub...)
		}
		n, err := New(t.TempDir(), Config{Seed: []byte("cap-det"), Validators: vals, MaxTxsPerBlock: 3, PoolCapacity: 2})
		if err != nil {
			t.Fatal(err)
		}
		var decisions []string
		submit := func(tx *Transaction) {
			res, err := n.Submit(tx)
			if err != nil {
				decisions = append(decisions, "reject:"+reason(err))
				return
			}
			decisions = append(decisions, "accept:"+res.TxID+":"+res.EvictedID+":"+res.ReplacedID)
		}
		for r := 1; r <= 3; r++ {
			submit(NewTransaction(keys[r%4].priv, uint64(r), []byte(fmt.Sprintf("c%d", r)), uint64((r*7)%13)+1, 100))
			submit(NewTransaction(keys[(r+1)%4].priv, 1, []byte(fmt.Sprintf("d%d", r)), uint64(r), 100))
			submit(NewTransaction(keys[(r+2)%4].priv, uint64(r), []byte(fmt.Sprintf("e%d", r)), uint64(r*3), 100))
			p, _ := n.Propose()
			confirmByVotes(t, n, keys, p.BlockID)
		}
		var blocks []string
		for _, b := range n.st2Blocks() {
			blocks = append(blocks, b.ID)
		}
		return decisions, blocks
	}
	d1, b1 := run()
	d2, b2 := run()
	if fmt.Sprint(d1) != fmt.Sprint(d2) {
		t.Fatalf("accept/evict decisions differ:\n%v\n%v", d1, d2)
	}
	if fmt.Sprint(b1) != fmt.Sprint(b2) {
		t.Fatalf("confirmed blocks differ:\n%v\n%v", b1, b2)
	}
}

// dropped 历史不会随后变成 expired。
func TestDroppedNeverExpires(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 1)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 3) // 若不退出池，进入轮次 3 时到期
	n.Submit(a)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 2, 100)
	res, err := n.Submit(b) // 挤出 a
	if err != nil || res.EvictedID != a.ID() {
		t.Fatalf("eviction: %+v %v", res, err)
	}
	if _, err := n.EndRound(); err != nil { // 轮次 2
		t.Fatal(err)
	}
	if _, err := n.EndRound(); err != nil { // 轮次 3
		t.Fatal(err)
	}
	info, _ := n.Tx(a.ID())
	if info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 1 {
		t.Fatalf("dropped tx must not become expired: %+v", info)
	}
}
