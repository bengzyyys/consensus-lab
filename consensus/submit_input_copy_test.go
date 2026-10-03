package consensus

import (
	"fmt"
	"testing"
)

// 本文件保障 Submit 的“提交输入独立性”：节点接受一笔已签名交易后，
// 调用方继续持有并修改原来的 *Transaction，节点接受的始终是提交成功时的内容。
// 与 query_copy_test.go 的查询结果副本保障分别守住出入口两侧：
// 提交入口拷贝输入，查询出口拷贝内部对象。
//
// 两类调用方编辑都要覆盖：
//   - 原地改写（in-place）：直接改原对象中的公钥、内容、签名字节与标量字段；
//   - 整体替换（replace）：把原对象中的这些字段换成另一份值/另一个数组。
// 两种编辑都只能改变调用方手中的对象，不能反过来改写池内交易。

// replaceAllFields 把原对象里的字段“换成另一份值”：Sender/Content/Signature
// 指向全新分配的数组（而非在原底层数组上改写），标量字段一并替换。
func replaceAllFields(tx *Transaction, sender, content, signature []byte, sequence, fee, expiry uint64) {
	tx.Sender = append([]byte(nil), sender...)
	tx.Content = append([]byte(nil), content...)
	tx.Signature = append([]byte(nil), signature...)
	tx.Sequence = sequence
	tx.Fee = fee
	tx.Expiry = expiry
}

// assertPendingAt 断言某账户当前恰有一笔序号为 seq、标识为 id 的待处理交易，
// 且其等待说明与给定状态相符；同时确认已确认序号与最早缺口序号。
func assertPendingAt(t *testing.T, n *Node, key testKey, confirmed, gap uint64, id, note string) {
	t.Helper()
	acct := n.Account(key.pub)
	if acct.ConfirmedSequence != confirmed || acct.Gap != gap || len(acct.Pending) != 1 {
		t.Fatalf("account %x = {confirmed:%d gap:%d pending:%d}, want {confirmed:%d gap:%d pending:1}",
			key.pub, acct.ConfirmedSequence, acct.Gap, len(acct.Pending), confirmed, gap)
	}
	got := acct.Pending[0]
	if got.ID != id || got.Note != note {
		t.Fatalf("pending entry = {id:%s note:%s}, want {id:%s note:%s}", got.ID, got.Note, id, note)
	}
}

// submitAndSnapshot 提交交易并返回提交成功时的独立快照，供事后与节点状态比对，
// 避免直接保留原对象指针——调用方随后会故意改写原对象。
func submitAndSnapshot(t *testing.T, n *Node, tx *Transaction) (*Transaction, string) {
	t.Helper()
	res, err := n.Submit(tx)
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	saved := copyTx(tx)
	if res.TxID != saved.ID() {
		t.Fatalf("submit returned id %s, want %s", res.TxID, saved.ID())
	}
	return saved, res.TxID
}

// 成功提交后，无论调用方在原对象上原地改写还是整体替换发送者公钥、序号、内容、
// 费用、到期轮次与签名字节，按提交返回标识查到的始终是接受时的完整交易：
// 标识与完整交易相符，签名仍有效；交易仍列在原发送者名下、保持原序号与缺口说明，
// 既不会搬到另一个账户，也不会凭空产生第二条待处理记录。
func TestSubmitInputIsIndependentCopy(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 调用方提交后继续持有原来的对象指针，提交成功时留一份独立快照作比对基准。
	callerTx := NewTransaction(keys[0].priv, 1, []byte("payload"), 4, 100)
	original, id := submitAndSnapshot(t, n, callerTx)

	// 账户初始形态：原发送者名下一笔序号 1 的待处理交易，无缺口。
	assertPendingAt(t, n, keys[0], 0, 0, id, "waiting-pack")
	otherAcct := n.Account(keys[1].pub)
	if otherAcct.ConfirmedSequence != 0 || otherAcct.Gap != 0 || len(otherAcct.Pending) != 0 {
		t.Fatalf("unrelated account must be empty before tamper: %+v", otherAcct)
	}

	// 第一类编辑：在原对象上原地改写公钥、内容、签名字节（原底层数组被改），
	// 并改序号、费用、到期轮次。
	callerTx.Sender[0] ^= 0xff
	callerTx.Content = append(callerTx.Content, "-in-place"...)
	callerTx.Signature[0] ^= 0xff
	callerTx.Sequence = 9
	callerTx.Fee = 777
	callerTx.Expiry = 1 // 改为当前轮次，试图让交易立即到期

	got, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued {
		t.Fatalf("status after in-place edit = %s, want %s", got.Status, StatusQueued)
	}
	assertTxMatches(t, got.Tx, original)
	// 交易没有搬到“被改成的公钥”名下，也没有出现第二条待处理记录。
	assertPendingAt(t, n, keys[0], 0, 0, id, "waiting-pack")
	if acct := n.Account(callerTx.Sender); len(acct.Pending) != 0 {
		t.Fatalf("in-place sender edit created a pending record under another account: %+v", acct.Pending)
	}
	if acct := n.Account(keys[1].pub); len(acct.Pending) != 0 {
		t.Fatalf("unrelated account must stay empty: %+v", acct.Pending)
	}

	// 第二类编辑：把这些字段整体换成另一份值（新数组、新标量）。
	replaceAllFields(callerTx,
		append([]byte(nil), keys[1].pub...),
		[]byte("whole-replacement"),
		append([]byte(nil), original.Signature...), // 先换成原签名的另一份拷贝……
		42, 1, 5)
	callerTx.Signature[0] ^= 0xaa // ……再破坏这份拷贝，原签名数组不受影响。

	got2, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	assertTxMatches(t, got2.Tx, original)
	assertPendingAt(t, n, keys[0], 0, 0, id, "waiting-pack")
	if acct := n.Account(keys[1].pub); len(acct.Pending) != 0 {
		t.Fatalf("replaced sender edit moved the tx to another account: %+v", acct.Pending)
	}
	// 篡改出来的标识没有成为节点中的另一笔交易。
	if _, err := n.Tx(callerTx.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("tampered id got %v, want %s", err, ReasonUnknownTx)
	}
}

// 序号缺口同样只按提交时的序号记录：提交一笔越过缺口的高序号交易后，
// 调用方把原对象序号改成缺口序号或更大的序号，账户的缺口说明与待处理记录不变。
func TestSubmitInputSequenceGapUnaffected(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 只提交序号 3：最早缺口序号为 1。
	callerTx := NewTransaction(keys[0].priv, 3, []byte("gappy"), 2, 100)
	original, id := submitAndSnapshot(t, n, callerTx)

	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 1 || len(acct.Pending) != 1 || acct.Pending[0].ID != id {
		t.Fatalf("setup account wrong: %+v", acct)
	}

	// 原地把序号改成缺口序号 1：不能凭空补上缺口，也不能让原交易越过缺口可打包。
	callerTx.Sequence = 1
	again := n.Account(keys[0].pub)
	if again.Gap != 1 || len(again.Pending) != 1 || again.Pending[0].Tx.Sequence != 3 || again.Pending[0].ID != id {
		t.Fatalf("in-place sequence edit leaked into account: %+v", again)
	}
	info, _ := n.Tx(id)
	assertTxMatches(t, info.Tx, original)

	// 整体替换成更大的序号 99：缺口与待处理记录仍是提交时的样子。
	replaceAllFields(callerTx, append([]byte(nil), keys[0].pub...),
		[]byte("gappy"), append([]byte(nil), original.Signature...), 99, 2, 100)
	again2 := n.Account(keys[0].pub)
	if again2.Gap != 1 || len(again2.Pending) != 1 || again2.Pending[0].Tx.Sequence != 3 {
		t.Fatalf("replaced sequence edit leaked into account: %+v", again2)
	}

	// 节点仍认为序号 1、2 缺失：空块提议证明高序号交易无法越过缺口被打包。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TxIDs) != 0 {
		t.Fatalf("tx must not cross the sequence gap after caller edit: %v", p.TxIDs)
	}
}

// 事后提高或降低输入对象的费用，不能改变原先由费用与交易标识决定的打包顺序。
// 原交易与其他账户的交易一起排队时，提议冻结的顺序始终是提交时费用对应的顺序。
func TestSubmitInputFeeEditsDoNotChangePackingOrder(t *testing.T) {
	keys := genKeys(t, 4)

	// 原交易 txA 费用 3、txB 费用 7：打包顺序固定为 [txB, txA]。
	setup := func() (*Node, *Transaction, *Transaction, *Transaction, string, string) {
		n, _ := newTestNode(t, keys, 10)
		callerA := NewTransaction(keys[0].priv, 1, []byte("alpha"), 3, 100)
		txB := NewTransaction(keys[1].priv, 1, []byte("beta"), 7, 100)
		origA, idA := submitAndSnapshot(t, n, callerA)
		if _, err := n.Submit(txB); err != nil {
			t.Fatal(err)
		}
		return n, callerA, txB, origA, idA, txB.ID()
	}

	t.Run("raise fee in place cannot jump the queue", func(t *testing.T) {
		n, callerA, _, origA, idA, idB := setup()
		callerA.Fee = 1000 // 原地把费用抬到最高。
		callerA.Content = append(callerA.Content, "!"...)
		callerA.Signature[0] ^= 0xff
		p, err := n.Propose()
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{idB, idA}) {
			t.Fatalf("packing order changed by in-place fee raise: %v", p.TxIDs)
		}
		info, _ := n.Tx(idA)
		assertTxMatches(t, info.Tx, origA)
	})

	t.Run("lower fee by replacement cannot sink the tx", func(t *testing.T) {
		n, callerA, _, origA, idA, idB := setup()
		// 整体替换：费用降为 0，内容与签名换成另一份数组。
		replaceAllFields(callerA, append([]byte(nil), origA.Sender...),
			[]byte("alpha-cheap"), make([]byte, 64), 1, 0, 100)
		p, err := n.Propose()
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{idB, idA}) {
			t.Fatalf("packing order changed by replaced fee lower: %v", p.TxIDs)
		}
		info, _ := n.Tx(idA)
		assertTxMatches(t, info.Tx, origA)
	})
}

// 事后改动输入对象的到期轮次，不能让原交易提前失效，也不能延长其有效期。
// 节点始终按提交时的到期轮次在进入新轮次时失效该交易。
func TestSubmitInputExpiryEditsDoNotChangeLifetime(t *testing.T) {
	keys := genKeys(t, 4)

	// 原交易到期轮次 3：第 2 轮仍有效，进入第 3 轮失效。
	t.Run("shrinking expiry in place must not expire it early", func(t *testing.T) {
		n, _ := newTestNode(t, keys, 10)
		callerTx := NewTransaction(keys[0].priv, 1, []byte("exp"), 1, 3)
		original, id := submitAndSnapshot(t, n, callerTx)

		callerTx.Expiry = 1 // 原地改为当前轮次，试图让交易立即到期。
		callerTx.Signature[0] ^= 0xff
		// 结束第 1 轮进入第 2 轮：按提交时的到期轮次 3，交易仍有效并回到排队。
		if _, err := n.EndRound(); err != nil {
			t.Fatal(err)
		}
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusQueued {
			t.Fatalf("tx expired early from in-place expiry edit: status = %s, want queued", info.Status)
		}
		assertTxMatches(t, info.Tx, original)
		p, err := n.Propose()
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{id}) {
			t.Fatalf("valid tx must still pack in round 2: %v", p.TxIDs)
		}

		// 结束第 2 轮进入第 3 轮：交易按原到期轮次失效，篡改不能延长有效期。
		if _, err := n.EndRound(); err != nil {
			t.Fatal(err)
		}
		expired, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if expired.Status != StatusExpired {
			t.Fatalf("status = %s, want expired at round 3", expired.Status)
		}
		assertTxMatches(t, expired.Tx, original)
		if acct := n.Account(keys[0].pub); len(acct.Pending) != 0 {
			t.Fatalf("expired tx must leave pending: %+v", acct.Pending)
		}
	})

	t.Run("extending expiry by replacement must not keep it alive", func(t *testing.T) {
		n, _ := newTestNode(t, keys, 10)
		callerTx := NewTransaction(keys[0].priv, 1, []byte("exp"), 1, 3)
		original, id := submitAndSnapshot(t, n, callerTx)

		// 整体替换：到期轮次延长到 1000，签名换成另一份数组。
		replaceAllFields(callerTx, append([]byte(nil), original.Sender...),
			append([]byte(nil), original.Content...), make([]byte, 64), 1, 1, 1000)
		if _, err := n.EndRound(); err != nil {
			t.Fatal(err)
		}
		if _, err := n.EndRound(); err != nil {
			t.Fatal(err)
		}
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusExpired {
			t.Fatalf("tx lifetime extended by caller replacement: status = %s, want expired", info.Status)
		}
		assertTxMatches(t, info.Tx, original)
	})
}

// 对原本能够打包且尚未到期的交易，事后修改输入对象不影响后续打包与确认：
// 确认块包含的仍是提交返回的原标识，交易查询关联实际确认的区块，
// 账户已确认序号按原交易推进；序号连续、单块上限与投票确认规则保持不变。
func TestSubmitInputEditsSurviveThroughConfirmation(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)

	// 账户甲序号 1、2 连续两笔，账户乙一笔参与同块；上限充足可同块打包。
	callerA1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 2, 100)
	callerA2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 8, 100)
	txB1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	origA1, idA1 := submitAndSnapshot(t, n, callerA1)
	origA2, idA2 := submitAndSnapshot(t, n, callerA2)
	if _, err := n.Submit(txB1); err != nil {
		t.Fatal(err)
	}
	idB1 := txB1.ID()

	// 提交成功后尽力改写甲手中的两个原对象（原地 + 整体替换两种手法）。
	callerA1.Sender[0] ^= 0xff
	callerA1.Content = append(callerA1.Content, "-x"...)
	callerA1.Signature[0] ^= 0xff
	callerA1.Fee = 9999 // 试图抬高费用插到最前
	callerA1.Sequence = 3
	callerA1.Expiry = 1
	replaceAllFields(callerA2, append([]byte(nil), keys[2].pub...),
		[]byte("a2-replaced"), make([]byte, 64), 77, 0, 1)

	// 打包顺序只由提交时的费用、标识与序号连续规则决定：首轮各账户下一条中
	// b1(5) 先于 a1(2)，a1 入选后 a2(8) 才具备条件，顺序固定为 [b1, a1, a2]；
	// 调用方把 a1 费用改成 9999 也不能让它插队，单块上限与序号连续规则照常生效。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{idB1, idA1, idA2}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("proposal order = %v, want %v (caller edits must not leak)", p.TxIDs, want)
	}
	if wantID := BlockID(1, 1, "", want); p.BlockID != wantID {
		t.Fatalf("block id = %s, want %s", p.BlockID, wantID)
	}

	// 投票确认规则不变：3/4 票严格超过 2/3 立即确认。
	confirmByVotes(t, n, keys, p.BlockID)

	// 确认块包含的仍是提交返回的原标识，交易查询关联实际确认的区块。
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(blk.TxIDs) != fmt.Sprint(want) || blk.ID != p.BlockID || blk.Height != 1 || blk.Round != 1 {
		t.Fatalf("confirmed block wrong: %+v", blk)
	}
	for _, id := range want {
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != p.BlockID {
			t.Fatalf("confirmed tx %s lookup wrong: %+v", id, info)
		}
	}
	assertTxMatches(t, n.mustTx(t, idA1).Tx, origA1)
	assertTxMatches(t, n.mustTx(t, idA2).Tx, origA2)

	// 账户已确认序号按原交易推进到 2；乙推进到 1；被改成的“另一个账户”一无所有。
	if acct := n.Account(keys[0].pub); acct.ConfirmedSequence != 2 || len(acct.Pending) != 0 {
		t.Fatalf("sender A account wrong after confirm: %+v", acct)
	}
	if acct := n.Account(keys[1].pub); acct.ConfirmedSequence != 1 || len(acct.Pending) != 0 {
		t.Fatalf("sender B account wrong after confirm: %+v", acct)
	}
	if acct := n.Account(keys[2].pub); acct.ConfirmedSequence != 0 || len(acct.Pending) != 0 {
		t.Fatalf("edits must not create state under the replaced sender: %+v", acct)
	}

	// 篡改不随正常保存落盘：重开后确认历史与账户推进仍是接受时的交易。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	storedBlk, err := reopened.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(storedBlk.TxIDs) != fmt.Sprint(want) || storedBlk.ID != p.BlockID {
		t.Fatalf("confirmed history changed after reopen: %+v", storedBlk)
	}
	info, err := reopened.Tx(idA1)
	if err != nil {
		t.Fatal(err)
	}
	assertTxMatches(t, info.Tx, origA1)
	if acct := reopened.Account(keys[0].pub); acct.ConfirmedSequence != 2 {
		t.Fatalf("confirmed sequence after reopen = %d, want 2", acct.ConfirmedSequence)
	}
}

// mustTx 按标识查询，失败即终止测试。
func (n *Node) mustTx(t *testing.T, id string) *TxInfo {
	t.Helper()
	info, err := n.Tx(id)
	if err != nil {
		t.Fatalf("tx %s: %v", id, err)
	}
	return info
}

// 空内容同样是合法交易。提交成功后再给原对象填入内容，节点中保留原来的
// 空内容、标识与有效签名；整体替换 Content 数组同样只影响调用方手中的对象。
func TestSubmitEmptyContentInputStaysIntact(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)

	callerTx := NewTransaction(keys[0].priv, 1, nil, 2, 100)
	original, id := submitAndSnapshot(t, n, callerTx)
	if len(original.Content) != 0 {
		t.Fatal("setup: content must be empty")
	}

	info, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != id || len(info.Tx.Content) != 0 {
		t.Fatalf("empty-content accepted tx wrong: %+v", info)
	}
	assertTxMatches(t, info.Tx, original)

	// 原地给原对象填入内容（Content 由 nil 变为有字节）。
	callerTx.Content = append(callerTx.Content, []byte("filled-by-caller")...)
	got, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tx.Content) != 0 || got.ID != id {
		t.Fatalf("in-place fill leaked into node: %+v", got)
	}
	assertTxMatches(t, got.Tx, original)

	// 整体替换 Content 为另一份非空数组。
	callerTx.Content = append([]byte(nil), []byte("replaced-content")...)
	callerTx.Signature[0] ^= 0xff
	got2, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Tx.Content) != 0 {
		t.Fatalf("replaced content leaked into node: %q", got2.Tx.Content)
	}
	assertTxMatches(t, got2.Tx, original)
	if acct := n.Account(keys[0].pub); len(acct.Pending) != 1 || acct.Pending[0].ID != id || len(acct.Pending[0].Tx.Content) != 0 {
		t.Fatalf("account pending changed by fill: %+v", acct.Pending)
	}

	// 空内容交易仍可正常打包与确认，确认块与查询都指向原空内容交易。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{id}) {
		t.Fatalf("empty-content tx proposal wrong: %v", p.TxIDs)
	}
	confirmByVotes(t, n, keys, p.BlockID)
	confirmed, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != StatusConfirmed || confirmed.BlockID != p.BlockID || len(confirmed.Tx.Content) != 0 {
		t.Fatalf("empty-content tx confirm wrong: %+v", confirmed)
	}
	assertTxMatches(t, confirmed.Tx, original)

	// 重开后仍是那笔空内容交易。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Tx.Content) != 0 || stored.Status != StatusConfirmed {
		t.Fatalf("empty-content tx changed after reopen: %+v", stored)
	}
	assertTxMatches(t, stored.Tx, original)
}

// 调用方把修改过、签名已经不匹配的对象再次提交，必须按现有规则以
// bad-signature 拒绝；之前接受的交易及其查询结果保持原样，拒绝不产生第二条记录。
func TestResubmitTamperedInputRejectedBadSignature(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	callerTx := NewTransaction(keys[0].priv, 1, []byte("once"), 3, 100)
	original, id := submitAndSnapshot(t, n, callerTx)

	t.Run("in-place tamper then resubmit", func(t *testing.T) {
		// 只改内容：签名不再覆盖新内容，验签失败。
		callerTx.Content = append(callerTx.Content, "-tampered"...)
		if _, err := n.Submit(callerTx); reason(err) != ReasonBadSignature {
			t.Fatalf("resubmit tampered content got %v, want %s", err, ReasonBadSignature)
		}
		// 只改费用：签名覆盖费用，同样验签失败（不会走到 fee-not-higher）。
		callerTx.Content = append([]byte(nil), original.Content...)
		callerTx.Fee = original.Fee + 1
		if _, err := n.Submit(callerTx); reason(err) != ReasonBadSignature {
			t.Fatalf("resubmit tampered fee got %v, want %s", err, ReasonBadSignature)
		}
	})

	t.Run("whole-replacement tamper then resubmit", func(t *testing.T) {
		// 换成另一账户的公钥但保留原签名：签名对新公钥无效，按 bad-signature 拒绝，
		// 而不是被当作另一账户的新交易接受。
		replaceAllFields(callerTx, append([]byte(nil), keys[1].pub...),
			append([]byte(nil), original.Content...),
			append([]byte(nil), original.Signature...),
			original.Sequence, original.Fee, original.Expiry)
		if _, err := n.Submit(callerTx); reason(err) != ReasonBadSignature {
			t.Fatalf("resubmit with replaced sender got %v, want %s", err, ReasonBadSignature)
		}
	})

	// 之前接受的交易及其查询结果保持原样。
	info, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued {
		t.Fatalf("original tx status = %s, want queued after rejected resubmits", info.Status)
	}
	assertTxMatches(t, info.Tx, original)
	// 没有产生第二条待处理记录，也没有搬到其他账户。
	assertPendingAt(t, n, keys[0], 0, 0, id, "waiting-pack")
	if acct := n.Account(keys[1].pub); len(acct.Pending) != 0 {
		t.Fatalf("rejected resubmit created a pending record: %+v", acct.Pending)
	}

	// 用原私钥重新签发“加费替换”是合法的现有行为：新交易被接受、旧交易标记
	// replaced，证明前面的拒绝纯粹来自签名规则，提交入口的兼容语义没有改变。
	replacement := NewTransaction(keys[0].priv, 1, []byte("once-bumped"), original.Fee+1, 100)
	res, err := n.Submit(replacement)
	if err != nil {
		t.Fatalf("properly signed higher-fee replacement should be accepted: %v", err)
	}
	if res.ReplacedID != id {
		t.Fatalf("replacement replaced id = %q, want %s", res.ReplacedID, id)
	}
	old, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != StatusReplaced || old.ReplacedBy != res.TxID {
		t.Fatalf("original tx after valid replacement = %+v, want replaced by %s", old, res.TxID)
	}
	assertTxMatches(t, old.Tx, original)
}

// 提交入口本身必须完成输入与内部存储的剥离，而不能依赖后续 Propose/Vote 的
// clone 兜底：成功提交后立即检查内部 Entries 中的交易，其指针与三个字节切片
// 都不与调用方对象共享底层数组；调用方随后在原对象上的任何编辑，到达内部
// 对象的路径必须全部被切断。
func TestSubmitDetachesInternalEntryFromCallerInput(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	callerTx := NewTransaction(keys[0].priv, 1, []byte("detach"), 6, 100)
	res, err := n.Submit(callerTx)
	if err != nil {
		t.Fatal(err)
	}
	stored := n.st.Entries[res.TxID].Tx
	if stored == callerTx {
		t.Fatal("Submit must store a distinct Transaction, not the caller's pointer")
	}
	// 三个字节切片都必须拥有独立底层数组：比较首元素地址即可识别别名。
	if len(callerTx.Sender) > 0 && &stored.Sender[0] == &callerTx.Sender[0] {
		t.Fatal("stored Sender shares backing array with caller input")
	}
	if len(callerTx.Content) > 0 && &stored.Content[0] == &callerTx.Content[0] {
		t.Fatal("stored Content shares backing array with caller input")
	}
	if len(callerTx.Signature) > 0 && &stored.Signature[0] == &callerTx.Signature[0] {
		t.Fatal("stored Signature shares backing array with caller input")
	}
	assertTxMatches(t, stored, callerTx)
}
