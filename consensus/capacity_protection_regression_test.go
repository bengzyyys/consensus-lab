package consensus

import (
	"fmt"
	"testing"
)

// mixedProtectionPool 是一个满池（容量 5）的混合场景：三笔被未决候选引用的
// 低费交易（仅本地提议、仅登记候选、被两个候选共同引用）与两笔未被任何候选
// 引用的同费排队交易。保护必须同时作用于三类引用，且受保护交易的费用不构成
// 接收新交易的门槛。
type mixedProtectionPool struct {
	n          *Node
	keys       []testKey
	protLocal  *Transaction // 仅本地提议引用，fee 1
	protShared *Transaction // 被两个登记候选共同引用，fee 3
	protOther  *Transaction // 仅登记候选引用，fee 2
	evA        *Transaction // 未引用排队，fee 5
	evB        *Transaction // 未引用排队，fee 5
	victim     *Transaction // evA、evB 中标识字典序最大者：唯一的淘汰对象
	survivor   *Transaction // 另一笔同费排队交易
	localID    string       // 本地提议区块标识
	sharedID   string       // 仅含 protShared 的候选区块标识
	combinedID string       // 含 protShared 与 protOther 的候选区块标识
}

func newMixedProtectionPool(t *testing.T) *mixedProtectionPool {
	t.Helper()
	keys := genKeys(t, 7)
	n, _ := newTestNodeCap(t, keys, 10, 5)

	// 本地提议只含 protLocal。
	protLocal := NewTransaction(keys[0].priv, 1, []byte("prot-local"), 1, 100)
	if _, err := n.Submit(protLocal); err != nil {
		t.Fatal(err)
	}
	prop, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(prop.TxIDs) != 1 || prop.TxIDs[0] != protLocal.ID() {
		t.Fatalf("local proposal = %v, want [protLocal]", prop.TxIDs)
	}

	// protShared 被两个登记候选共同引用；protOther 只被其中一个引用。
	protShared := NewTransaction(keys[1].priv, 1, []byte("prot-shared"), 3, 100)
	protOther := NewTransaction(keys[2].priv, 1, []byte("prot-other"), 2, 100)
	for _, tx := range []*Transaction{protShared, protOther} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	rcShared, err := n.RegisterCandidate(1, []string{protShared.ID()})
	if err != nil {
		t.Fatal(err)
	}
	rcCombined, err := n.RegisterCandidate(1, []string{protShared.ID(), protOther.ID()})
	if err != nil {
		t.Fatal(err)
	}

	// 已取得票：本地提议与共享候选各一票，后续提交与淘汰不得影响。
	if _, err := n.Vote(keys[0].pub, 1, prop.BlockID); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Vote(keys[1].pub, 1, rcShared.BlockID); err != nil {
		t.Fatal(err)
	}

	// 第 4、5 笔：未被引用的排队交易，池满。
	evA := NewTransaction(keys[3].priv, 1, []byte("ev-a"), 5, 100)
	evB := NewTransaction(keys[4].priv, 1, []byte("ev-b"), 5, 100)
	for _, tx := range []*Transaction{evA, evB} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	victim, survivor := evA, evB
	if evB.ID() > evA.ID() {
		victim, survivor = evB, evA
	}

	return &mixedProtectionPool{
		n:          n,
		keys:       keys,
		protLocal:  protLocal,
		protShared: protShared,
		protOther:  protOther,
		evA:        evA,
		evB:        evB,
		victim:     victim,
		survivor:   survivor,
		localID:    prop.BlockID,
		sharedID:   rcShared.BlockID,
		combinedID: rcCombined.BlockID,
	}
}

// assertProposalAndCandidatesIntact 校验本地提议、全部竞争候选的交易顺序、
// 区块标识与已取得的票均未改变。
func assertProposalAndCandidatesIntact(t *testing.T, mp *mixedProtectionPool) {
	t.Helper()
	n := mp.n

	prop, ok := n.Proposal()
	if !ok || prop.BlockID != mp.localID || len(prop.TxIDs) != 1 || prop.TxIDs[0] != mp.protLocal.ID() {
		t.Fatalf("local proposal changed: %+v ok=%v", prop, ok)
	}

	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || len(rc.Candidates) != 3 {
		t.Fatalf("candidates changed: %+v", rc)
	}
	byID := map[string]CandidateView{}
	for _, c := range rc.Candidates {
		byID[c.BlockID] = c
	}
	wantVoters := map[string][][]byte{
		mp.localID:  {mp.keys[0].pub},
		mp.sharedID: {mp.keys[1].pub},
	}
	wantTxs := map[string][]string{
		mp.localID:    {mp.protLocal.ID()},
		mp.sharedID:   {mp.protShared.ID()},
		mp.combinedID: {mp.protShared.ID(), mp.protOther.ID()},
	}
	for id, txs := range wantTxs {
		c, ok := byID[id]
		if !ok {
			t.Fatalf("candidate %s missing", id)
		}
		if c.Result != CandidatePending || fmt.Sprint(c.TxIDs) != fmt.Sprint(txs) {
			t.Fatalf("candidate %s changed: %+v", id, c)
		}
		voters := []string{}
		for _, v := range c.Voters {
			voters = append(voters, fmt.Sprintf("%x", v))
		}
		want := []string{}
		for _, v := range wantVoters[id] {
			want = append(want, fmt.Sprintf("%x", v))
		}
		if fmt.Sprint(voters) != fmt.Sprint(want) {
			t.Fatalf("candidate %s voters changed: got %v, want %v", id, voters, want)
		}
	}
}

// assertProtectedIntact 校验三笔受保护交易仍在等待投票，且所属账户待处理列表不变。
func assertProtectedIntact(t *testing.T, mp *mixedProtectionPool) {
	t.Helper()
	protected := []struct {
		key int
		tx  *Transaction
	}{
		{0, mp.protLocal},
		{1, mp.protShared},
		{2, mp.protOther},
	}
	for _, p := range protected {
		info, err := mp.n.Tx(p.tx.ID())
		if err != nil || info.Status != StatusProposed {
			t.Fatalf("protected tx %s = %v, %v; want proposed", p.tx.ID(), info, err)
		}
		acct := mp.n.Account(mp.keys[p.key].pub)
		if acct.ConfirmedSequence != 0 || len(acct.Pending) != 1 ||
			acct.Pending[0].ID != p.tx.ID() || acct.Pending[0].Note != "waiting-vote" {
			t.Fatalf("protected sender account changed: %+v", acct)
		}
	}
}

// assertMixedPoolIntact 校验整个满池保持提交前结果：受保护交易等待投票、
// 两笔排队交易等待打包、提议与候选（含票数）不变。
func assertMixedPoolIntact(t *testing.T, mp *mixedProtectionPool) {
	t.Helper()
	assertProtectedIntact(t, mp)
	for i, tx := range []*Transaction{mp.evA, mp.evB} {
		info, err := mp.n.Tx(tx.ID())
		if err != nil || info.Status != StatusQueued {
			t.Fatalf("queued tx %s = %v, %v; want queued", tx.ID(), info, err)
		}
		acct := mp.n.Account(mp.keys[3+i].pub)
		if acct.ConfirmedSequence != 0 || len(acct.Pending) != 1 ||
			acct.Pending[0].ID != tx.ID() || acct.Pending[0].Note != "waiting-pack" {
			t.Fatalf("queued sender account changed: %+v", acct)
		}
	}
	assertProposalAndCandidatesIntact(t, mp)
}

// 受保护交易的费用不构成接收门槛：新交易费用高于全部受保护交易、却低于所有
// 可淘汰交易时必须以 pool-full 拒绝——既不能挤出候选中的低费交易，也不能仅
// 因为比受保护交易贵就被接收。与可淘汰对象同费但标识更大时结果相同。
// 拒绝不返回成功结果、查询仍为 unknown-transaction、不留淘汰记录，
// 已有交易、账户待处理列表、提议与候选保持提交前结果。
func TestCapacityProtectedFeeNotThreshold(t *testing.T) {
	mp := newMixedProtectionPool(t)
	n := mp.n

	// 费用 4：高于全部受保护交易（1、2、3），低于唯一可淘汰的费用档位（5）。
	mid := NewTransaction(mp.keys[5].priv, 1, []byte("mid"), 4, 100)
	res, err := n.Submit(mid)
	if err == nil || res != nil {
		t.Fatalf("mid-fee submit = %+v, %v; want nil result with rejection", res, err)
	}
	if reason(err) != ReasonPoolFull {
		t.Fatalf("mid-fee submit got %v, want %s", err, ReasonPoolFull)
	}
	if _, err := n.Tx(mid.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx left history: %v", err)
	}
	assertMixedPoolIntact(t, mp)

	// 与可淘汰对象同费（5）但标识更大：同样 pool-full。
	var larger *Transaction
	for i := 0; larger == nil; i++ {
		tx := NewTransaction(mp.keys[5].priv, uint64(i+1), []byte(fmt.Sprintf("larger-%d", i)), 5, 100)
		if tx.ID() > mp.victim.ID() {
			larger = tx
		}
	}
	res, err = n.Submit(larger)
	if err == nil || res != nil {
		t.Fatalf("same-fee larger-id submit = %+v, %v; want nil result with rejection", res, err)
	}
	if reason(err) != ReasonPoolFull {
		t.Fatalf("same-fee larger-id submit got %v, want %s", err, ReasonPoolFull)
	}
	if _, err := n.Tx(larger.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx left history: %v", err)
	}
	assertMixedPoolIntact(t, mp)
}

// 满池淘汰只从未被候选引用的排队交易中选择：费用更高的新交易挤出的是可淘汰
// 交易中费用最低（同费取标识字典序最大）的一笔，费用更低的受保护交易全部保留；
// 同费但标识更小的新交易同样只挤出可淘汰对象。被挤出交易保留完整内容并显示
// dropped/pool-capacity/发生轮次，淘汰不推进已确认序号；提议、候选与票数不受影响。
func TestCapacityEvictionSkipsProtected(t *testing.T) {
	mp := newMixedProtectionPool(t)
	n := mp.n

	// 费用更高（6 > 5）：挤出两笔同费可淘汰交易中标识较大的 victim，
	// 而不是任何一笔费用更低的受保护交易。
	higher := NewTransaction(mp.keys[5].priv, 1, []byte("higher"), 6, 100)
	res, err := n.Submit(higher)
	if err != nil {
		t.Fatalf("higher-fee submit should evict the evictable tx: %v", err)
	}
	if res.TxID != higher.ID() || res.EvictedID != mp.victim.ID() || res.ReplacedID != "" {
		t.Fatalf("eviction result wrong: %+v", res)
	}

	// 被挤出交易：dropped / pool-capacity / 发生轮次 1，保留完整内容，
	// 不带确认块或替换关联。
	info, err := n.Tx(mp.victim.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 1 {
		t.Fatalf("dropped tx wrong: %+v", info)
	}
	if info.BlockHeight != 0 || info.BlockID != "" || info.ReplacedBy != "" {
		t.Fatalf("dropped tx must not carry block/replace links: %+v", info)
	}
	if info.Tx == nil || string(info.Tx.Content) != string(mp.victim.Content) || info.Tx.Fee != 5 {
		t.Fatalf("dropped tx content not preserved: %+v", info.Tx)
	}
	// 它从所属账户的待处理列表中退出，已确认序号不因此推进。
	if acct := n.Account(mp.victim.Sender); acct.ConfirmedSequence != 0 || len(acct.Pending) != 0 {
		t.Fatalf("evicted sender account wrong: %+v", acct)
	}

	// 新交易成为排队交易，提交结果不带替换关联（上面已查 ReplacedID 为空）。
	if info, _ := n.Tx(higher.ID()); info.Status != StatusQueued {
		t.Fatalf("new tx = %s, want queued", info.Status)
	}
	if acct := n.Account(mp.keys[5].pub); len(acct.Pending) != 1 ||
		acct.Pending[0].ID != higher.ID() || acct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("new sender account wrong: %+v", acct)
	}

	// 接收与淘汰不涉及其他交易：幸存排队交易与全部受保护交易保持原样。
	if info, _ := n.Tx(mp.survivor.ID()); info.Status != StatusQueued {
		t.Fatalf("survivor = %s, want queued", info.Status)
	}
	assertProtectedIntact(t, mp)
	assertProposalAndCandidatesIntact(t, mp)
	if n.CurrentRound() != 1 {
		t.Fatalf("round advanced to %d, want 1", n.CurrentRound())
	}

	// 同费但标识更小：可淘汰对象现为 survivor（fee 5）与 higher（fee 6），
	// 费用最低的是 survivor；构造 fee 5 且标识小于 survivor 的新交易挤出它。
	var smaller *Transaction
	for i := 0; smaller == nil; i++ {
		tx := NewTransaction(mp.keys[6].priv, uint64(i+1), []byte(fmt.Sprintf("smaller-%d", i)), 5, 100)
		if tx.ID() < mp.survivor.ID() {
			smaller = tx
		}
	}
	res, err = n.Submit(smaller)
	if err != nil {
		t.Fatalf("same-fee smaller-id submit should evict: %v", err)
	}
	if res.TxID != smaller.ID() || res.EvictedID != mp.survivor.ID() || res.ReplacedID != "" {
		t.Fatalf("eviction result wrong: %+v", res)
	}
	info, err = n.Tx(mp.survivor.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 1 {
		t.Fatalf("dropped tx wrong: %+v", info)
	}
	if info.Tx == nil || string(info.Tx.Content) != string(mp.survivor.Content) || info.Tx.Fee != 5 {
		t.Fatalf("dropped tx content not preserved: %+v", info.Tx)
	}
	if acct := n.Account(mp.survivor.Sender); acct.ConfirmedSequence != 0 || len(acct.Pending) != 0 {
		t.Fatalf("evicted sender account wrong: %+v", acct)
	}
	if info, _ := n.Tx(smaller.ID()); info.Status != StatusQueued {
		t.Fatalf("new tx = %s, want queued", info.Status)
	}

	// higher 与全部受保护交易不受影响，提议、候选与票数保持原样。
	if info, _ := n.Tx(higher.ID()); info.Status != StatusQueued {
		t.Fatalf("higher = %s, want queued", info.Status)
	}
	assertProtectedIntact(t, mp)
	assertProposalAndCandidatesIntact(t, mp)
}
