package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住“合法加费替换”的原子落盘回归保障。
//
// 场景：两笔交易来自同一发送者、使用同一正整数序号，签名有效且尚未到期；
// 旧交易已在池中排队，新交易内容不同、费用严格更高，旧交易未被任何未决候选
// 引用，因此提交本应作为一次合法替换被接收。但当这次提交因保存节点状态失败
// 而结束时，替换必须整体不生效：提交返回保存错误本身（基础设施错误，而非
// 拒绝原因），结果为空——不能同时给出成功接收或成功替换的结果；旧交易查询
// 仍显示排队，保留原内容、费用与标识，替代交易关联为空；新交易查询仍得到
// 交易不存在的拒绝原因，不留排队记录或替换历史；账户查询中的待处理交易仍是
// 旧交易，已确认序号与原有缺口不变，该账户后续序号的交易继续按原序号排队。
// 磁盘状态文件逐字节不变、不残留临时文件，重新打开原状态目录看到与当前查询
// 完全一致的操作前状态——不能出现当前查询恢复了旧交易、重开却看到新交易
// 已替换成功的差异。
//
// 池容量用满的情况纳入同一项保障：合法加费替换不新增占位，满池也可执行；
// 保存失败同样不能为了给新交易腾位置而挤出其他交易，其他账户的待处理记录
// 与淘汰关联保持原样。
//
// 保存恢复正常后，再提交刚才失败的同一笔新交易，必须按一次正常替换成功处理，
// 不能因失败尝试残留记录而被认作重复交易：结果关联被替换的旧标识、不带被挤出
// 标识；旧交易此时才显示已被替换并指向新交易，新交易成为该序号唯一的待处理
// 交易；账户已确认序号仍不推进，池满时也不影响其他账户。
// 本组测试不新增公开入口，也不改变现有替换规则。

// assertReplaceRolledBack 断言加费替换保存失败后（或失败前）的完整操作前状态：
// 旧交易仍排队、保留原内容且替代关联为空；新交易无任何记录；账户待处理交易
// 仍是旧交易与后续序号交易，已确认序号与缺口不变。
func assertReplaceRolledBack(t *testing.T, n *Node, oldTx, newTx, later, gapped *Transaction, sender []byte) {
	t.Helper()

	// 旧交易：排队状态、原内容/费用/标识原样，替代交易关联为空。
	info := n.mustTx(t, oldTx.ID())
	if info.Status != StatusQueued {
		t.Fatalf("old tx status = %s, want queued", info.Status)
	}
	if info.ReplacedBy != "" {
		t.Fatalf("old tx must not reference a replacement, got ReplacedBy=%q", info.ReplacedBy)
	}
	assertTxMatches(t, info.Tx, oldTx)

	// 新交易：不留排队记录或替换历史，查询得到交易不存在的拒绝原因。
	if _, err := n.Tx(newTx.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("failed replacement must leave no record of the new tx: %v, want %s", err, ReasonUnknownTx)
	}

	// 账户查询：待处理交易仍是旧交易，后续序号交易按原序号排队，
	// 已确认序号与原有缺口（缺 seq3）不发生变化。
	assertAccountQueue(t, n, sender, 0, map[string]string{
		oldTx.ID():  "waiting-pack",
		later.ID():  "waiting-pack",
		gapped.ID(): "waiting-pack",
	})
	acct := n.Account(sender)
	if acct.Gap != 3 {
		t.Fatalf("account gap = %d, want 3 (missing seq 3)", acct.Gap)
	}
	wantOrder := []string{oldTx.ID(), later.ID(), gapped.ID()}
	var gotOrder []string
	for _, p := range acct.Pending {
		gotOrder = append(gotOrder, p.ID)
	}
	if fmt.Sprint(gotOrder) != fmt.Sprint(wantOrder) {
		t.Fatalf("pending order = %v, want %v (later sequences keep their slots)", gotOrder, wantOrder)
	}
}

// assertReplaceCommitted 断言保存恢复后再次提交同一笔新交易成功替换后的状态：
// 旧交易显示已被替换并指向新交易；新交易成为该序号唯一的待处理交易；
// 后续序号交易保持排队；已确认序号与缺口不推进。
func assertReplaceCommitted(t *testing.T, n *Node, oldTx, newTx, later, gapped *Transaction, sender []byte) {
	t.Helper()

	// 旧交易此时才显示已被替换，并指向新交易标识。
	oldInfo := n.mustTx(t, oldTx.ID())
	if oldInfo.Status != StatusReplaced {
		t.Fatalf("old tx status = %s, want replaced", oldInfo.Status)
	}
	if oldInfo.ReplacedBy != newTx.ID() {
		t.Fatalf("old tx ReplacedBy = %q, want %q", oldInfo.ReplacedBy, newTx.ID())
	}
	assertTxMatches(t, oldInfo.Tx, oldTx)

	// 新交易在池中排队，内容完整。
	newInfo := n.mustTx(t, newTx.ID())
	if newInfo.Status != StatusQueued {
		t.Fatalf("new tx status = %s, want queued", newInfo.Status)
	}
	assertTxMatches(t, newInfo.Tx, newTx)

	// 新交易成为该序号唯一的待处理交易；后续序号交易不被删除、不被占据位置；
	// 已确认序号仍不推进，缺口不变。
	assertAccountQueue(t, n, sender, 0, map[string]string{
		newTx.ID():  "waiting-pack",
		later.ID():  "waiting-pack",
		gapped.ID(): "waiting-pack",
	})
	acct := n.Account(sender)
	if acct.Gap != 3 {
		t.Fatalf("account gap after replacement = %d, want 3", acct.Gap)
	}
	for _, p := range acct.Pending {
		if p.ID == oldTx.ID() {
			t.Fatalf("replaced old tx must not stay pending: %+v", acct.Pending)
		}
	}
}

// assertStateFileUntouched 断言失败的保存没有改写状态文件，也没有残留临时文件。
func assertStateFileUntouched(t *testing.T, dir string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("state file changed despite the failed save")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[state.json]" {
		t.Fatalf("failed save must not leave temp files, dir contains %v", names)
	}
}

// TestReplaceSaveFailureAtomic 覆盖合法加费替换保存失败的完整回归序列：
// 失败整体回滚（保存错误、结果为空）-> 内存/磁盘/重开三处一致保持操作前状态 ->
// 恢复后再次提交同一笔新交易按正常替换成功，而非被认作重复交易。
func TestReplaceSaveFailureAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)

	// 旧交易已排队；同账户另有后续序号交易（seq2）与带缺口的 seq4，
	// 用于验证失败不会删除它们、也不会让失败的新交易占据位置。
	oldTx := NewTransaction(keys[0].priv, 1, []byte("old"), 5, 100)
	later := NewTransaction(keys[0].priv, 2, []byte("later"), 7, 100)
	gapped := NewTransaction(keys[0].priv, 4, []byte("gapped"), 1, 100)
	for _, tx := range []*Transaction{oldTx, later, gapped} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit: %v", err)
		}
	}
	// 新交易：同发送者同序号、内容不同、费用严格更高，签名有效且未到期；
	// 未产生任何提议，旧交易未被未决候选引用，替换原本合法。
	newTx := NewTransaction(keys[0].priv, 1, []byte("new"), 9, 100)
	if newTx.ID() == oldTx.ID() {
		t.Fatal("setup: replacement must have a different id")
	}
	assertReplaceRolledBack(t, n, oldTx, newTx, later, gapped, keys[0].pub)

	// 快照替换前的磁盘文件：失败的保存不得改动它。
	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 合法替换在保存节点状态失败时结束：必须返回保存错误本身，
	// 不能包装成拒绝原因，也不能给出任何成功结果。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.Submit(newTx)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("replacement submit must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if res != nil {
		t.Fatalf("submit result must be nil on save failure, got %+v", res)
	}

	// 内存：旧交易、新交易、账户查询全部保持操作前结果。
	assertReplaceRolledBack(t, n, oldTx, newTx, later, gapped, keys[0].pub)

	// 磁盘：状态文件逐字节不变，不残留临时文件。
	assertStateFileUntouched(t, dir, before)

	// 重开节点：看到与当前查询完全一致的操作前状态——旧交易仍在排队，
	// 新交易无记录，不存在“内存回滚了但磁盘已替换成功”的差异。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertReplaceRolledBack(t, reopened, oldTx, newTx, later, gapped, keys[0].pub)

	// 保存恢复正常后，再提交刚才失败的同一笔新交易：按一次正常替换成功处理，
	// 不能因失败尝试残留记录而被认作重复交易。
	res, err = reopened.Submit(newTx)
	if err != nil {
		t.Fatalf("resubmit after recovery must succeed as a normal replacement: %v", err)
	}
	if res.TxID != newTx.ID() || res.ReplacedID != oldTx.ID() || res.EvictedID != "" {
		t.Fatalf("replacement result = %+v, want TxID=%s ReplacedID=%s EvictedID empty",
			res, newTx.ID(), oldTx.ID())
	}
	assertReplaceCommitted(t, reopened, oldTx, newTx, later, gapped, keys[0].pub)

	// 替换结果随再次落盘持久化：再次重开看到完全一致的状态。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertReplaceCommitted(t, durable, oldTx, newTx, later, gapped, keys[0].pub)
}

// TestReplaceSaveFailurePoolFullAtomic 把池容量用满的情况纳入同一项保障：
// 合法加费替换不新增占位，满池也可执行；保存失败不能为了给新交易腾位置而
// 挤出其他账户的交易；恢复后再次提交按正常替换成功，仍不淘汰任何交易。
func TestReplaceSaveFailurePoolFullAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNodeCap(t, keys, 10, 3)

	// 三个账户各一笔排队交易，池恰好占满（3/3）。
	oldTx := NewTransaction(keys[0].priv, 1, []byte("old"), 5, 100)
	otherB := NewTransaction(keys[1].priv, 1, []byte("b"), 3, 100)
	otherC := NewTransaction(keys[2].priv, 1, []byte("c"), 4, 100)
	for _, tx := range []*Transaction{oldTx, otherB, otherC} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit: %v", err)
		}
	}
	// sanity：池确已用满——费用不占优的全新交易只能得到 pool-full。
	stranger := NewTransaction(keys[3].priv, 1, []byte("stranger"), 1, 100)
	if _, err := n.Submit(stranger); reason(err) != ReasonPoolFull {
		t.Fatalf("setup: pool must be full, fresh low-fee tx got %v, want %s", err, ReasonPoolFull)
	}

	// 满池下的合法加费替换：同发送者同序号、费用严格更高，不新增占位，
	// 也不需要挤出任何交易。
	newTx := NewTransaction(keys[0].priv, 1, []byte("new"), 50, 100)

	// assertPoolIntact 断言三笔原交易全部保持排队、无任何淘汰或替换关联，
	// 新交易无记录；各账户已确认序号均为 0。
	assertPoolIntact := func(t *testing.T, n *Node) {
		t.Helper()
		info := n.mustTx(t, oldTx.ID())
		if info.Status != StatusQueued || info.ReplacedBy != "" {
			t.Fatalf("old tx = %+v, want queued with no replacement link", info)
		}
		assertTxMatches(t, info.Tx, oldTx)
		for _, tx := range []*Transaction{otherB, otherC} {
			oi := n.mustTx(t, tx.ID())
			if oi.Status != StatusQueued {
				t.Fatalf("other account tx %s status = %s, want queued (must not be evicted for the failed replacement)", tx.ID(), oi.Status)
			}
			if oi.DropReason != "" || oi.DropRound != 0 || oi.ReplacedBy != "" {
				t.Fatalf("other account tx %s must not carry any eviction/replacement record: %+v", tx.ID(), oi)
			}
		}
		if _, err := n.Tx(newTx.ID()); reason(err) != ReasonUnknownTx {
			t.Fatalf("failed replacement must leave no record of the new tx: %v, want %s", err, ReasonUnknownTx)
		}
		assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{oldTx.ID(): "waiting-pack"})
		assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{otherB.ID(): "waiting-pack"})
		assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{otherC.ID(): "waiting-pack"})
	}
	assertPoolIntact(t, n)

	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 满池下的合法替换因保存失败而结束：保存错误、结果为空。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.Submit(newTx)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("full-pool replacement must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if res != nil {
		t.Fatalf("submit result must be nil on save failure, got %+v", res)
	}

	// 内存/磁盘/重开三处一致：没有为了给新交易腾位置而挤出其他交易。
	assertPoolIntact(t, n)
	assertStateFileUntouched(t, dir, before)
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertPoolIntact(t, reopened)

	// 恢复后再次提交同一笔新交易：正常替换成功，结果关联旧标识、不带被挤出
	// 标识；池满也不影响其他账户。
	res, err = reopened.Submit(newTx)
	if err != nil {
		t.Fatalf("resubmit after recovery must succeed as a normal replacement: %v", err)
	}
	if res.TxID != newTx.ID() || res.ReplacedID != oldTx.ID() || res.EvictedID != "" {
		t.Fatalf("replacement result = %+v, want TxID=%s ReplacedID=%s EvictedID empty",
			res, newTx.ID(), oldTx.ID())
	}

	// assertPoolReplaced 断言替换成功后：旧交易指向新交易，新交易排队，
	// 其他账户两笔交易保持排队、无淘汰关联，已确认序号均不推进。
	assertPoolReplaced := func(t *testing.T, n *Node) {
		t.Helper()
		oldInfo := n.mustTx(t, oldTx.ID())
		if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != newTx.ID() {
			t.Fatalf("old tx after replacement = %+v, want replaced by %s", oldInfo, newTx.ID())
		}
		newInfo := n.mustTx(t, newTx.ID())
		if newInfo.Status != StatusQueued {
			t.Fatalf("new tx status = %s, want queued", newInfo.Status)
		}
		assertTxMatches(t, newInfo.Tx, newTx)
		for _, tx := range []*Transaction{otherB, otherC} {
			oi := n.mustTx(t, tx.ID())
			if oi.Status != StatusQueued || oi.DropReason != "" || oi.DropRound != 0 {
				t.Fatalf("other account tx %s must stay queued without eviction record: %+v", tx.ID(), oi)
			}
		}
		assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{newTx.ID(): "waiting-pack"})
		assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{otherB.ID(): "waiting-pack"})
		assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{otherC.ID(): "waiting-pack"})
	}
	assertPoolReplaced(t, reopened)

	// 替换结果持久化：再次重开看到完全一致的满池替换后状态。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertPoolReplaced(t, durable)
}
