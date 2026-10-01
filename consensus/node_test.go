package consensus

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type testKey struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func genKeys(t *testing.T, n int) []testKey {
	t.Helper()
	keys := make([]testKey, n)
	for i := range keys {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = testKey{priv: priv, pub: pub}
	}
	return keys
}

func newTestNode(t *testing.T, keys []testKey, maxTxs uint64) (*Node, string) {
	t.Helper()
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("test-seed-v1"), Validators: vals, MaxTxsPerBlock: maxTxs})
	if err != nil {
		t.Fatal(err)
	}
	return n, dir
}

func reason(err error) string {
	var re *RejectError
	if errors.As(err, &re) {
		return re.Reason
	}
	return ""
}

// 初始状态：轮次 1，账户确认序号 0，配置必须校验。
func TestInitialState(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	if n.CurrentRound() != 1 {
		t.Fatalf("initial round = %d, want 1", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("initial height = %d, want 0", n.Height())
	}
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 {
		t.Fatalf("initial confirmed seq = %d, want 0", acct.ConfirmedSequence)
	}
	if acct.Gap != 0 || len(acct.Pending) != 0 {
		t.Fatalf("unexpected pending: %+v", acct)
	}

	dir := t.TempDir()
	cfg := Config{Seed: []byte("s"), Validators: [][]byte{keys[0].pub}, MaxTxsPerBlock: 1}
	if _, err := New(dir, Config{Validators: cfg.Validators, MaxTxsPerBlock: 1}); err == nil {
		t.Fatal("empty seed should be rejected")
	}
	if _, err := New(t.TempDir(), Config{Seed: []byte("s"), MaxTxsPerBlock: 1}); err == nil {
		t.Fatal("empty validators should be rejected")
	}
	if _, err := New(t.TempDir(), Config{Seed: []byte("s"), Validators: cfg.Validators, MaxTxsPerBlock: 0}); err == nil {
		t.Fatal("zero max txs should be rejected")
	}
	if _, err := New(t.TempDir(), Config{Seed: []byte("s"), Validators: [][]byte{keys[0].pub, keys[0].pub}, MaxTxsPerBlock: 1}); err == nil {
		t.Fatal("duplicate validators should be rejected")
	}
}

// 签名编码与交易标识稳定。
func TestStableEncoding(t *testing.T) {
	keys := genKeys(t, 1)
	tx := NewTransaction(keys[0].priv, 7, []byte("hello"), 3, 99)
	id1, sb1 := tx.ID(), tx.SigningBytes()
	// 重新逐字节构造一份相同内容的交易，标识与签名编码必须一致。
	tx2 := NewTransaction(keys[0].priv, 7, []byte("hello"), 3, 99)
	if tx2.ID() != id1 {
		t.Fatal("same bytes must yield same id")
	}
	if string(tx2.SigningBytes()) != string(sb1) {
		t.Fatal("signing bytes must be stable")
	}
	if !tx2.Verify() {
		t.Fatal("valid signature should verify")
	}
	// 篡改内容后签名失效。
	tx2.Content = []byte("tampered")
	if tx2.Verify() {
		t.Fatal("tampered content must fail verification")
	}
	// 签名必须覆盖费用、到期轮次与序号。
	txFee := NewTransaction(keys[0].priv, 7, []byte("hello"), 3, 99)
	txFee.Fee++
	if txFee.Verify() {
		t.Fatal("tampered fee must fail verification")
	}
	txExp := NewTransaction(keys[0].priv, 7, []byte("hello"), 3, 99)
	txExp.Expiry++
	if txExp.Verify() {
		t.Fatal("tampered expiry must fail verification")
	}
	txSeq := NewTransaction(keys[0].priv, 7, []byte("hello"), 3, 99)
	txSeq.Sequence++
	if txSeq.Verify() {
		t.Fatal("tampered sequence must fail verification")
	}
	// 不同费用产生不同标识。
	tx3 := NewTransaction(keys[0].priv, 7, []byte("hello"), 4, 99)
	if tx3.ID() == id1 {
		t.Fatal("different fee must yield different id")
	}
}

// 各种拒绝原因。
func TestSubmitRejections(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 非法签名
	bad := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 10)
	bad.Signature[0] ^= 0xff
	if _, err := n.Submit(bad); reason(err) != ReasonBadSignature {
		t.Fatalf("got %v, want %s", err, ReasonBadSignature)
	}

	// 序号非正
	zero := NewTransaction(keys[0].priv, 0, []byte("a"), 1, 10)
	if _, err := n.Submit(zero); reason(err) != "sequence-must-be-positive" {
		t.Fatalf("got %v, want sequence-must-be-positive", err)
	}

	// 已到期：到期轮次不大于当前轮次 1
	exp := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 1)
	if _, err := n.Submit(exp); reason(err) != ReasonExpired {
		t.Fatalf("got %v, want %s", err, ReasonExpired)
	}

	ok := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 10)
	res, err := n.Submit(ok)
	if err != nil {
		t.Fatal(err)
	}

	// 完全相同的交易重复提交
	if _, err := n.Submit(NewTransaction(keys[0].priv, 1, []byte("a"), 1, 10)); reason(err) != ReasonDuplicate {
		t.Fatalf("got %v, want %s", err, ReasonDuplicate)
	}

	// 同序号费用不更高
	low := NewTransaction(keys[0].priv, 1, []byte("b"), 1, 10)
	if _, err := n.Submit(low); reason(err) != ReasonLowFee {
		t.Fatalf("got %v, want %s", err, ReasonLowFee)
	}

	// 费用严格更高才能替换
	hi := NewTransaction(keys[0].priv, 1, []byte("b"), 5, 10)
	rep, err := n.Submit(hi)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReplacedID != res.TxID || rep.TxID != hi.ID() {
		t.Fatalf("replacement result wrong: %+v", rep)
	}
	oldInfo, err := n.Tx(res.TxID)
	if err != nil {
		t.Fatal(err)
	}
	if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != hi.ID() {
		t.Fatalf("old tx status = %s replacedBy=%s, want replaced/%s", oldInfo.Status, oldInfo.ReplacedBy, hi.ID())
	}
	newInfo, _ := n.Tx(hi.ID())
	if newInfo.Status != StatusQueued {
		t.Fatalf("new tx status = %s, want queued", newInfo.Status)
	}
}

// 确认后序号不大于已确认序号被拒；缺口交易可入池等待，查询指出最早缺口。
func TestGapAndConfirmedSequence(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	tx2 := NewTransaction(keys[0].priv, 2, []byte("two"), 1, 100)
	if _, err := n.Submit(tx2); err != nil {
		t.Fatal(err)
	}
	acct := n.Account(keys[0].pub)
	if acct.Gap != 1 || len(acct.Pending) != 1 {
		t.Fatalf("gap = %d pending = %d, want gap 1", acct.Gap, len(acct.Pending))
	}
	if acct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("note = %q, want waiting-pack", acct.Pending[0].Note)
	}

	tx1 := NewTransaction(keys[0].priv, 1, []byte("one"), 1, 100)
	if _, err := n.Submit(tx1); err != nil {
		t.Fatal(err)
	}
	acct = n.Account(keys[0].pub)
	if acct.Gap != 0 {
		t.Fatalf("gap = %d, want 0", acct.Gap)
	}

	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TxIDs) != 2 || p.TxIDs[0] != tx1.ID() || p.TxIDs[1] != tx2.ID() {
		t.Fatalf("block order wrong: %v", p.TxIDs)
	}
	confirmByVotes(t, n, keys, p.BlockID)
	if n.CurrentRound() != 2 {
		t.Fatalf("round = %d, want 2", n.CurrentRound())
	}

	acct = n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 2 {
		t.Fatalf("confirmed = %d, want 2", acct.ConfirmedSequence)
	}
	// 序号不大于已确认序号
	replay := NewTransaction(keys[0].priv, 2, []byte("two"), 1, 100)
	if _, err := n.Submit(replay); reason(err) != ReasonDuplicate {
		t.Fatalf("same tx replay got %v, want duplicate", err)
	}
	oldSeq := NewTransaction(keys[0].priv, 1, []byte("one"), 9, 100)
	if _, err := n.Submit(oldSeq); reason(err) != ReasonOldSequence {
		t.Fatalf("got %v, want %s", err, ReasonOldSequence)
	}
}

// 打包顺序：跨账户按费用，同账户后续序号必须等前一条被选入；达到上限停止。
func TestPackingOrder(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 2)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 100, 100) // 费用最高，但必须等 a1 选入后才可考虑
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
	for _, tx := range []*Transaction{a1, a2, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	// 第一次取各账户下一条：b1(50) 高于 a1(10)；第二次只有 a1 可确认，
	// a2 因序号缺口本轮取不到，且已达上限 2。
	want := []string{b1.ID(), a1.ID()}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("packed %v, want %v", p.TxIDs, want)
	}
	// 确认后下一轮 a2 成为该账户下一条可确认交易并被打包
	confirmByVotes(t, n, keys, p.BlockID)
	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.TxIDs) != 1 || p2.TxIDs[0] != a2.ID() {
		t.Fatalf("round 2 packed %v, want [a2]", p2.TxIDs)
	}
}

// 费用相同时按交易标识字典序选择（确定性可复算）。
func TestTieBreakByID(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 1)
	txs := make([]*Transaction, 4)
	for i := range txs {
		txs[i] = NewTransaction(keys[i].priv, 1, []byte("x"), 7, 100)
		if _, err := n.Submit(txs[i]); err != nil {
			t.Fatal(err)
		}
	}
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	minID := txs[0].ID()
	for _, tx := range txs[1:] {
		if tx.ID() < minID {
			minID = tx.ID()
		}
	}
	if len(p.TxIDs) != 1 || p.TxIDs[0] != minID {
		t.Fatalf("picked %v, want lexicographically smallest %s", p.TxIDs, minID)
	}
}

// 重复请求返回同一提议；提议冻结后新交易不改变提议，提议中交易禁止替换。
func TestProposalStableAndFrozen(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx1 := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	if _, err := n.Submit(tx1); err != nil {
		t.Fatal(err)
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if p1.BlockID != p2.BlockID || len(p2.TxIDs) != 1 || p2.TxIDs[0] != tx1.ID() {
		t.Fatal("repeated proposal must be identical")
	}
	// 提议后再提交的交易不改变当前提议
	tx2 := NewTransaction(keys[1].priv, 1, []byte("b"), 99, 100)
	if _, err := n.Submit(tx2); err != nil {
		t.Fatal(err)
	}
	p3, _ := n.Propose()
	if len(p3.TxIDs) != 1 {
		t.Fatalf("proposal changed after new submit: %v", p3.TxIDs)
	}
	// 提议中的交易禁止替换
	replace := NewTransaction(keys[0].priv, 1, []byte("a2"), 999, 100)
	if _, err := n.Submit(replace); reason(err) != ReasonProposalLocked {
		t.Fatalf("got %v, want %s", err, ReasonProposalLocked)
	}
	info, _ := n.Tx(tx1.ID())
	if info.Status != StatusProposed {
		t.Fatalf("tx in proposal status = %s, want proposed", info.Status)
	}
	acct := n.Account(keys[0].pub)
	if len(acct.Pending) != 1 || acct.Pending[0].Status != StatusProposed || acct.Pending[0].Note != "waiting-vote" {
		t.Fatalf("proposed tx should show waiting-vote: %+v", acct.Pending)
	}
	// 未确认时结束轮次，交易回到排队状态
	r, err := n.EndRound()
	if err != nil {
		t.Fatal(err)
	}
	if r != 2 {
		t.Fatalf("round after end = %d, want 2", r)
	}
	acct = n.Account(keys[0].pub)
	if acct.Pending[0].Status != StatusQueued || acct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("returned tx should be queued/waiting-pack: %+v", acct.Pending[0])
	}
	if _, ok := n.Proposal(); ok {
		t.Fatal("proposal should be cleared after EndRound")
	}
	if n.Height() != 0 {
		t.Fatal("ended round must leave no confirmed block")
	}
}

// 空块：没有可选交易时返回空块并按相同条件确认。
func TestEmptyBlock(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty || len(p.TxIDs) != 0 {
		t.Fatalf("want empty proposal, got %+v", p)
	}
	emptyID := p.BlockID
	confirmByVotes(t, n, keys, p.BlockID)
	b, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != emptyID || len(b.TxIDs) != 0 || b.Height != 1 || b.Round != 1 {
		t.Fatalf("empty block wrong: %+v", b)
	}
	if b.PreviousID != "" {
		t.Fatalf("genesis previous id = %q, want empty", b.PreviousID)
	}
}

func confirmByVotes(t *testing.T, n *Node, keys []testKey, blockID string) {
	t.Helper()
	round := n.CurrentRound()
	// N=4 需 >2/3，即至少 3 票。
	count := len(keys)*2/3 + 1
	for i := 0; i < count; i++ {
		res, err := n.Vote(keys[i].pub, round, blockID)
		if err != nil {
			t.Fatalf("vote %d: %v", i, err)
		}
		if i < count-1 && res.Confirmed {
			t.Fatalf("confirmed too early at vote %d", i+1)
		}
		if i == count-1 {
			if !res.Confirmed || res.Block == nil {
				t.Fatalf("vote %d should confirm", i+1)
			}
		}
	}
}

// 投票阈值、重复投票、非法投票拒绝。
func TestVoting(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	p, _ := n.Propose()
	round := n.CurrentRound()

	// 名单外身份
	outsider := genKeys(t, 1)[0]
	if _, err := n.Vote(outsider.pub, round, p.BlockID); reason(err) != ReasonNotValidator {
		t.Fatalf("got %v, want %s", err, ReasonNotValidator)
	}
	// 错误轮次
	if _, err := n.Vote(keys[0].pub, round+1, p.BlockID); reason(err) != ReasonWrongRound {
		t.Fatalf("got %v, want %s", err, ReasonWrongRound)
	}
	// 错误区块标识
	if _, err := n.Vote(keys[0].pub, round, "deadbeef"); reason(err) != ReasonWrongBlock {
		t.Fatalf("got %v, want %s", err, ReasonWrongBlock)
	}

	// 一票
	res, err := n.Vote(keys[0].pub, round, p.BlockID)
	if err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("first vote: %+v %v", res, err)
	}
	// 重复投同一票不增加票数
	res, err = n.Vote(keys[0].pub, round, p.BlockID)
	if err != nil || res.Counted {
		t.Fatalf("duplicate vote: %+v %v", res, err)
	}
	// 第二票仍不确认（4 人需 3 票）
	res, _ = n.Vote(keys[1].pub, round, p.BlockID)
	if res.Confirmed {
		t.Fatal("2/4 votes must not confirm")
	}
	// 第三票确认
	res, _ = n.Vote(keys[2].pub, round, p.BlockID)
	if !res.Confirmed {
		t.Fatal("3/4 votes must confirm")
	}
	// 确认后旧轮次投票拒绝（轮次已前进）
	if _, err := n.Vote(keys[3].pub, round, p.BlockID); reason(err) != ReasonWrongRound {
		t.Fatalf("stale vote after confirm got %v, want wrong-round", err)
	}
}

// 链连续性：高度、前块标识、交易顺序，且交易查询关联区块。
func TestChainContinuity(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	tx1 := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	tx2 := NewTransaction(keys[1].priv, 1, []byte("b"), 2, 100)
	n.Submit(tx1)
	n.Submit(tx2)
	p1, _ := n.Propose()
	confirmByVotes(t, n, keys, p1.BlockID)

	tx3 := NewTransaction(keys[0].priv, 2, []byte("c"), 1, 100)
	n.Submit(tx3)
	p2, _ := n.Propose()
	confirmByVotes(t, n, keys, p2.BlockID)

	b1, _ := n.BlockAt(1)
	b2, _ := n.BlockAt(2)
	if b1.Height != 1 || b2.Height != 2 {
		t.Fatal("heights not continuous")
	}
	if b2.PreviousID != b1.ID {
		t.Fatalf("previous id %s != %s", b2.PreviousID, b1.ID)
	}
	if b1.TxIDs[0] != tx2.ID() || b1.TxIDs[1] != tx1.ID() {
		t.Fatalf("round 1 order wrong: %v", b1.TxIDs)
	}
	if b2.TxIDs[0] != tx3.ID() {
		t.Fatalf("round 2 order wrong: %v", b2.TxIDs)
	}
	latest, ok := n.LatestBlock()
	if !ok || latest.ID != b2.ID {
		t.Fatal("latest block wrong")
	}
	info, _ := n.Tx(tx1.ID())
	if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != b1.ID {
		t.Fatalf("tx lookup wrong: %+v", info)
	}
	if _, err := n.BlockAt(3); reason(err) != ReasonUnknownBlock {
		t.Fatalf("got %v, want unknown-block", err)
	}
	if _, err := n.Tx("nope"); reason(err) != ReasonUnknownTx {
		t.Fatalf("got %v, want unknown-tx", err)
	}
}

// 进入新轮次时到期交易失效；旧轮次投票不能影响新提议。
func TestExpiryAndStaleVotes(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	tx1 := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 3) // 在进入轮次 3 时失效
	n.Submit(tx1)
	p1, _ := n.Propose()
	// 不投票确认，直接结束轮次进入轮次 2，交易回池
	n.EndRound()
	p2, _ := n.Propose()
	if len(p2.TxIDs) != 1 || p2.TxIDs[0] != tx1.ID() {
		t.Fatalf("tx still valid in round 2: %v", p2.TxIDs)
	}
	// 旧轮次投票不能影响新提议
	if _, err := n.Vote(keys[0].pub, 1, p2.BlockID); reason(err) != ReasonWrongRound {
		t.Fatalf("got %v, want wrong-round", err)
	}
	if _, err := n.Vote(keys[0].pub, 2, p1.BlockID); reason(err) != ReasonWrongBlock {
		t.Fatalf("got %v, want wrong-block", err)
	}
	// 结束轮次 2 进入轮次 3，tx1 到期
	n.EndRound()
	info, _ := n.Tx(tx1.ID())
	if info.Status != StatusExpired {
		t.Fatalf("status = %s, want expired", info.Status)
	}
	p3, _ := n.Propose()
	if !p3.Empty {
		t.Fatalf("expired tx must not be packed: %v", p3.TxIDs)
	}
	acct := n.Account(keys[0].pub)
	if len(acct.Pending) != 0 {
		t.Fatalf("expired tx must not remain pending: %+v", acct.Pending)
	}
}

// 停止再启动恢复全部状态。
func TestPersistenceRoundTrip(t *testing.T) {
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("persist-seed"), Validators: vals, MaxTxsPerBlock: 5})
	if err != nil {
		t.Fatal(err)
	}
	txQ := NewTransaction(keys[0].priv, 2, []byte("gap"), 1, 100) // 缺口，留在池中
	n.Submit(txQ)
	tx1 := NewTransaction(keys[1].priv, 1, []byte("a"), 4, 100)
	n.Submit(tx1)
	p, _ := n.Propose()
	n.Vote(keys[0].pub, 1, p.BlockID) // 仅一票，提议与票数未决

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n2.CurrentRound() != 1 {
		t.Fatalf("round restored = %d", n2.CurrentRound())
	}
	p2, ok := n2.Proposal()
	if !ok || p2.BlockID != p.BlockID || len(p2.TxIDs) != len(p.TxIDs) {
		t.Fatalf("proposal not restored: %+v", p2)
	}
	// 重复投票不计数：keys[0] 的票已随状态恢复
	res, err := n2.Vote(keys[0].pub, 1, p.BlockID)
	if err != nil || res.Counted {
		t.Fatalf("restored vote dedup failed: %+v %v", res, err)
	}
	// 再补两票即可确认，池内缺口交易仍在
	res, _ = n2.Vote(keys[1].pub, 1, p.BlockID)
	res, _ = n2.Vote(keys[2].pub, 1, p.BlockID)
	if !res.Confirmed {
		t.Fatal("restored node should reach confirmation")
	}
	acct := n2.Account(keys[0].pub)
	if acct.Gap != 1 || len(acct.Pending) != 1 || acct.Pending[0].ID != txQ.ID() {
		t.Fatalf("pool not restored: %+v", acct)
	}
	// 确认历史与账户序号
	b, _ := n2.BlockAt(1)
	if b.ID != p.BlockID {
		t.Fatal("history not restored")
	}
	if a := n2.Account(keys[1].pub); a.ConfirmedSequence != 1 {
		t.Fatalf("confirmed seq restored = %d", a.ConfirmedSequence)
	}

	// New 拒绝覆盖已有状态目录
	if _, err := New(dir, Config{Seed: []byte("persist-seed"), Validators: vals, MaxTxsPerBlock: 5}); err == nil {
		t.Fatal("New over existing state must fail")
	}
}

// 确定性：相同种子、初始配置与输入顺序，产生相同提议者、交易顺序、区块标识与确认历史。
func TestDeterminism(t *testing.T) {
	keys := genKeys(t, 4)
	build := func() ([]string, []Block, []byte, uint64) {
		dir := t.TempDir()
		vals := make([][]byte, len(keys))
		for i, k := range keys {
			vals[i] = append([]byte(nil), k.pub...)
		}
		n, err := New(dir, Config{Seed: []byte("determinism-seed"), Validators: vals, MaxTxsPerBlock: 3})
		if err != nil {
			t.Fatal(err)
		}
		var proposers []byte
		for r := 1; r <= 3; r++ {
			proposers = append(proposers, n.CurrentProposer()...)
			// 固定的输入顺序
			n.Submit(NewTransaction(keys[r%4].priv, uint64(r), []byte(fmt.Sprintf("c%d", r)), uint64((r*7)%13), 100))
			n.Submit(NewTransaction(keys[(r+1)%4].priv, 1, []byte(fmt.Sprintf("d%d", r)), uint64(r), 100))
			p, _ := n.Propose()
			confirmByVotes(t, n, keys, p.BlockID)
		}
		var ids []string
		for _, b := range n.st2Blocks() {
			ids = append(ids, b.ID)
		}
		return ids, n.st2Blocks(), proposers, n.CurrentRound()
	}
	ids1, blocks1, prop1, round1 := build()
	ids2, blocks2, prop2, round2 := build()
	if fmt.Sprint(ids1) != fmt.Sprint(ids2) {
		t.Fatalf("block ids differ:\n%v\n%v", ids1, ids2)
	}
	if string(prop1) != string(prop2) {
		t.Fatal("proposers differ")
	}
	if round1 != round2 {
		t.Fatal("rounds differ")
	}
	for i := range blocks1 {
		if fmt.Sprint(blocks1[i].TxIDs) != fmt.Sprint(blocks2[i].TxIDs) {
			t.Fatalf("tx order differs in block %d", i+1)
		}
	}
}

// st2Blocks 是测试辅助：导出全部确认块。
func (n *Node) st2Blocks() []Block {
	out := make([]Block, len(n.st.Blocks))
	copy(out, n.st.Blocks)
	return out
}

// 提议者可查询且在名单内；同轮可复算。
func TestProposerQuery(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	p := n.CurrentProposer()
	found := false
	for _, k := range keys {
		if string(k.pub) == string(p) {
			found = true
		}
	}
	if !found {
		t.Fatal("proposer not in validator set")
	}
	if string(n.ProposerFor(1)) != string(p) {
		t.Fatal("ProposerFor(1) must equal current proposer")
	}
}

// 写入失败时一次确认不生效：内存保持确认前状态，磁盘也仍是确认前文件，
// 恢复后只能看到确认前的完整状态。
func TestConfirmAtomicityOnWriteFailure(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)
	tx1 := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	n.Submit(tx1)
	p, _ := n.Propose()
	// 已有 1 票的状态先落盘
	if _, err := n.Vote(keys[0].pub, 1, p.BlockID); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 注入写入失败：决定性的第三票必须报错，且状态保持确认前。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.Vote(keys[1].pub, 1, p.BlockID)
	if err == nil {
		t.Fatal("expected save error")
	}
	if res != nil {
		t.Fatalf("result should be nil on error, got %+v", res)
	}
	n.injectSaveErr = nil

	if n.Height() != 0 || n.CurrentRound() != 1 {
		t.Fatalf("in-memory state changed despite failed write: height=%d round=%d", n.Height(), n.CurrentRound())
	}
	pp, ok := n.Proposal()
	if !ok || pp.BlockID != p.BlockID {
		t.Fatal("proposal must survive the failed confirmation")
	}
	info, _ := n.Tx(tx1.ID())
	if info.Status != StatusProposed {
		t.Fatalf("tx status = %s, want proposed (pre-confirmation state)", info.Status)
	}
	// 磁盘文件仍是确认前内容
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("state file changed despite failed write")
	}
	// 模拟“中途停止后恢复”：Open 出来的也只能是确认前状态
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Height() != 0 || reopened.CurrentRound() != 1 {
		t.Fatal("reopened node should show pre-confirmation state")
	}
	// keys[1] 那次投票没有计入，仍需两票才能确认
	r2, err := reopened.Vote(keys[1].pub, 1, p.BlockID)
	if err != nil || r2.Confirmed {
		t.Fatalf("first post-reopen vote: %+v %v", r2, err)
	}
	r3, err := reopened.Vote(keys[2].pub, 1, p.BlockID)
	if err != nil || !r3.Confirmed {
		t.Fatalf("second post-reopen vote should confirm: %+v %v", r3, err)
	}
	if reopened.Height() != 1 {
		t.Fatal("confirmation should now be durable")
	}
}

// 保存文件形态检查：状态目录下只有 state.json（临时文件已收尾）。
func TestStateFileLayout(t *testing.T) {
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := [][]byte{keys[0].pub, keys[1].pub, keys[2].pub}
	n, err := New(filepath.Join(dir, "node"), Config{Seed: []byte("x"), Validators: vals, MaxTxsPerBlock: 2})
	if err != nil {
		t.Fatal(err)
	}
	n.Submit(NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100))
	p, _ := n.Propose()
	n.Vote(keys[0].pub, 1, p.BlockID)
	entries, err := os.ReadDir(filepath.Join(dir, "node"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[state.json]" {
		t.Fatalf("unexpected files: %v", names)
	}
}
