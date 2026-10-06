package consensus

import (
	"bytes"
	"fmt"
	"testing"
)

// 本文件回归保障：按轮次查看候选的查询 Candidates(round) 返回的是查询当时的
// 独立副本。调用者拿到结果后重排展示顺序、编辑候选标识/结果/交易列表、
// 改写投票者公钥字节或未投票名单，都只能改变自己手中的数据，不能改变节点
// 登记的候选、真实投票记录、另一份查询结果或确认历史。
//
// 重点覆盖同一轮存在本地提议与竞争候选、两者共享交易且分别已有投票，以及
// 空交易列表候选和尚无投票候选的边界情况。

// pubkeySet 取若干公钥的十六进制集合，便于与实际返回逐字节比较。
func pubkeySet(pubs ...[]byte) map[string]struct{} {
	set := make(map[string]struct{}, len(pubs))
	for _, p := range pubs {
		set[fmt.Sprintf("%x", p)] = struct{}{}
	}
	return set
}

// assertPubkeySet 断言实际公钥列表与期望集合完全一致；公钥按字典序返回，
// 因此这里同时校验返回顺序也已排好。
func assertPubkeySet(t *testing.T, got [][]byte, want map[string]struct{}, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %d keys (%x), want %d", what, len(got), got, len(want))
	}
	prev := ""
	for i, p := range got {
		h := fmt.Sprintf("%x", p)
		if h < prev {
			t.Fatalf("%s not sorted lexicographically at index %d: %x", what, i, got)
		}
		prev = h
		if _, ok := want[h]; !ok {
			t.Fatalf("%s contains unexpected key %x", what, p)
		}
	}
}

// assertCandidateState 按区块标识断言候选的本地标记、结果、真实交易顺序、
// 投票者集合；unvoted 非 nil 时一并校验整轮的未投票名单。
func assertCandidateState(t *testing.T, rc *RoundCandidates, blockID string, local bool, result string, txIDs []string, voters, unvoted map[string]struct{}) {
	t.Helper()
	c := candidateByBlockID(rc, blockID)
	if c == nil {
		t.Fatalf("candidate %s not found in round %d", blockID, rc.Round)
	}
	if c.Local != local || c.Result != result ||
		fmt.Sprint(c.TxIDs) != fmt.Sprint(txIDs) {
		t.Fatalf("candidate %s = {local:%v result:%s tx:%v}, want {local:%v result:%s tx:%v}",
			blockID, c.Local, c.Result, c.TxIDs, local, result, txIDs)
	}
	assertPubkeySet(t, c.Voters, voters, fmt.Sprintf("candidate %s voters", blockID))
	if unvoted != nil {
		assertPubkeySet(t, rc.Unvoted, unvoted, "unvoted list")
	}
}

// assertRoundCandidatesSorted 断言候选按区块标识字典序返回。
func assertRoundCandidatesSorted(t *testing.T, rc *RoundCandidates) {
	t.Helper()
	prev := ""
	for i, c := range rc.Candidates {
		if c.BlockID < prev {
			t.Fatalf("candidates not sorted at index %d: %v", i, [2]string{prev, c.BlockID})
		}
		prev = c.BlockID
	}
}

// snapshotRoundCandidates 对查询结果做一份与节点、与原结果都无关的深拷贝，
// 用于稍后比对“查询当时”的内容是否保留。
func snapshotRoundCandidates(rc *RoundCandidates) *RoundCandidates {
	if rc == nil {
		return nil
	}
	snap := &RoundCandidates{
		Round:          rc.Round,
		Candidates:     make([]CandidateView, len(rc.Candidates)),
		Unvoted:        make([][]byte, len(rc.Unvoted)),
		Ended:          rc.Ended,
		UnconfirmedEnd: rc.UnconfirmedEnd,
		HasRecords:     rc.HasRecords,
	}
	for i, c := range rc.Candidates {
		view := CandidateView{
			Round:   c.Round,
			BlockID: c.BlockID,
			TxIDs:   append([]string(nil), c.TxIDs...),
			Local:   c.Local,
			Voters:  make([][]byte, len(c.Voters)),
			Result:  c.Result,
		}
		for j, v := range c.Voters {
			view.Voters[j] = append([]byte(nil), v...)
		}
		snap.Candidates[i] = view
	}
	for i, v := range rc.Unvoted {
		snap.Unvoted[i] = append([]byte(nil), v...)
	}
	return snap
}

// assertSnapshotsEqual 逐字段断言两份轮次结果内容一致（不要求指针同一）。
func assertSnapshotsEqual(t *testing.T, got, want *RoundCandidates) {
	t.Helper()
	if got.Round != want.Round || got.Ended != want.Ended ||
		got.UnconfirmedEnd != want.UnconfirmedEnd || got.HasRecords != want.HasRecords ||
		len(got.Candidates) != len(want.Candidates) || len(got.Unvoted) != len(want.Unvoted) {
		t.Fatalf("snapshot header mismatch:\n got %+v\nwant %+v", got, want)
	}
	for i := range want.Candidates {
		g, w := got.Candidates[i], want.Candidates[i]
		if g.BlockID != w.BlockID || g.Local != w.Local || g.Result != w.Result ||
			fmt.Sprint(g.TxIDs) != fmt.Sprint(w.TxIDs) || len(g.Voters) != len(w.Voters) {
			t.Fatalf("candidate[%d] mismatch:\n got %+v\nwant %+v", i, g, w)
		}
		for j := range w.Voters {
			if !bytes.Equal(g.Voters[j], w.Voters[j]) {
				t.Fatalf("candidate %s voter[%d] = %x, want %x", w.BlockID, j, g.Voters[j], w.Voters[j])
			}
		}
	}
	for i := range want.Unvoted {
		if !bytes.Equal(got.Unvoted[i], want.Unvoted[i]) {
			t.Fatalf("unvoted[%d] = %x, want %x", i, got.Unvoted[i], want.Unvoted[i])
		}
	}
}

// tamperRoundCandidates 以调用方身份尽力改写手中的轮次结果：
// 改写每个候选标识、结果与本地标记、重排候选展示顺序、改写/交换/追加交易
// 标识、翻转投票者公钥字节并追加伪造投票者、翻转未投票公钥字节并追加伪造
// 名单。
func tamperRoundCandidates(rc *RoundCandidates) {
	for i := range rc.Candidates {
		c := &rc.Candidates[i]
		c.BlockID = fmt.Sprintf("%s-%d", fakeBlockID, i)
		c.Result = CandidateLost // 原本是 pending
		c.Local = !c.Local
		if len(c.TxIDs) > 0 {
			c.TxIDs[0] = fakeTxID
		}
		if len(c.TxIDs) > 1 {
			c.TxIDs[0], c.TxIDs[1] = c.TxIDs[1], c.TxIDs[0]
		}
		c.TxIDs = append(c.TxIDs, fakeTxID+"-cand")
		if len(c.Voters) > 0 {
			c.Voters[0][0] ^= 0xff
		}
		c.Voters = append(c.Voters, bytes.Repeat([]byte{0xde}, 32))
	}
	if len(rc.Candidates) > 1 {
		rc.Candidates[0], rc.Candidates[1] = rc.Candidates[1], rc.Candidates[0]
	}
	if len(rc.Unvoted) > 0 {
		rc.Unvoted[0][0] ^= 0xff
	}
	rc.Unvoted = append(rc.Unvoted, bytes.Repeat([]byte{0xad}, 32))
}

// assertResultWasTamperedByCaller 确认改写确实只发生在调用方手中这一份上：
// 区块标识全部变为伪造值、结果不再是 pending、列表与名单被追加。
func assertResultWasTamperedByCaller(t *testing.T, rc *RoundCandidates, realIDs []string) {
	t.Helper()
	if len(rc.Candidates) != len(realIDs) {
		t.Fatalf("caller copy candidates = %d, want %d", len(rc.Candidates), len(realIDs))
	}
	real := map[string]struct{}{}
	for _, id := range realIDs {
		real[id] = struct{}{}
	}
	for _, c := range rc.Candidates {
		if _, ok := real[c.BlockID]; ok {
			t.Fatalf("caller edit did not change held block id %s", c.BlockID)
		}
		if c.Result == CandidatePending {
			t.Fatalf("caller edit did not change held result for %s", c.BlockID)
		}
		if len(c.TxIDs) == 0 || c.TxIDs[len(c.TxIDs)-1] != fakeTxID+"-cand" {
			t.Fatalf("caller edit did not append to held tx list: %v", c.TxIDs)
		}
	}
}

// 主场景：同一轮本地提议含两笔分属不同账户的交易，竞争候选只含其中一笔，
// 两个候选共享该笔交易且分别已有一票，两名验证者尚未投票。连续取得两份
// 结果并尽力改写其中一份：另一份与节点真实记录保持原值；同一份结果内，共享
// 交易的两个候选也不因一方列表被编辑而改变另一方。本轮未结束时，编辑查询
// 结果不能伪造投票：真实验证者重复投原候选不增票、改投仍被拒绝且原票保留。
// 随后剩余两人投竞争候选：第二票不确认，第三票立即确认并进入下一轮；
// 新查询旧轮显示竞争候选胜出、本地提议落选、各自实际投票者、未投票为空；
// 先前保留的未编辑结果仍是查询当时（双方各一票、两候选未决、两人未投票）。
func TestCandidatesQueryIndependentCopiesSharedTx(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 2)

	// 两笔不同账户的交易：Alice 费用 20 在前，Bob 费用 10 在后。
	txAlice := NewTransaction(keys[0].priv, 1, []byte("alice"), 20, 100)
	txBob := NewTransaction(keys[1].priv, 1, []byte("bob"), 10, 100)
	for _, tx := range []*Transaction{txAlice, txBob} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	idA, idB := txAlice.ID(), txBob.ID()

	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	localIDs := []string{idA, idB}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint(localIDs) {
		t.Fatalf("setup: local proposal = %v, want %v", local.TxIDs, localIDs)
	}
	// 竞争候选只含共享的 Alice 交易。
	alt, err := n.RegisterCandidate(1, []string{idA})
	if err != nil {
		t.Fatal(err)
	}
	altIDs := []string{idA}
	if alt.BlockID == local.BlockID {
		t.Fatal("setup: competing candidate must differ from local proposal")
	}

	// 验证者 0 投本地提议，验证者 1 投竞争候选，其余两人未投票。
	if res, err := n.Vote(keys[0].pub, 1, local.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("local vote = %+v %v, want counted without confirm", res, err)
	}
	if res, err := n.Vote(keys[1].pub, 1, alt.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("alt vote = %+v %v, want counted without confirm", res, err)
	}

	// 连续取得两份结果：两个未决候选及各自真实交易顺序、投票者、未投票名单。
	first, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	saved := snapshotRoundCandidates(first) // 保留查询当时内容，之后不再编辑。
	second, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	twoUnvoted := pubkeySet(keys[2].pub, keys[3].pub)
	for _, rc := range []*RoundCandidates{first, second} {
		if rc.Round != 1 || rc.Ended || len(rc.Candidates) != 2 || !rc.HasRecords {
			t.Fatalf("pending round view wrong: %+v", rc)
		}
		assertRoundCandidatesSorted(t, rc)
		assertCandidateState(t, rc, local.BlockID, true, CandidatePending, localIDs,
			pubkeySet(keys[0].pub), twoUnvoted)
		assertCandidateState(t, rc, alt.BlockID, false, CandidatePending, altIDs,
			pubkeySet(keys[1].pub), twoUnvoted)
	}
	assertSnapshotsEqual(t, second, saved)

	// 改写第一份的候选标识、结果、交易列表、投票者公钥字节与未投票名单。
	tamperRoundCandidates(first)
	assertResultWasTamperedByCaller(t, first, []string{local.BlockID, alt.BlockID})

	// 另一份已取得但未编辑的结果保持原值。
	assertSnapshotsEqual(t, second, saved)

	// 同一份结果内，共享交易的两个候选不能互为别名：编辑竞争候选的列表与
	// 投票者字节，本地提议那条的真实交易顺序与投票者保持不变。
	var altView, localView *CandidateView
	for i := range second.Candidates {
		switch second.Candidates[i].BlockID {
		case alt.BlockID:
			altView = &second.Candidates[i]
		case local.BlockID:
			localView = &second.Candidates[i]
		}
	}
	if altView == nil || localView == nil {
		t.Fatalf("candidate views missing: %+v", second.Candidates)
	}
	altView.TxIDs[0] = fakeTxID
	altView.TxIDs = append(altView.TxIDs, fakeTxID+"-shared")
	altView.Voters[0][0] ^= 0xff
	if fmt.Sprint(localView.TxIDs) != fmt.Sprint(localIDs) {
		t.Fatalf("local list changed by alt-list edit within same result: %v", localView.TxIDs)
	}
	if fmt.Sprintf("%x", localView.Voters[0]) != fmt.Sprintf("%x", keys[0].pub) {
		t.Fatalf("local voter changed by alt-voter edit within same result: %x", localView.Voters[0])
	}

	// 再次查询仍应看到节点的真实记录，候选仍按区块标识排序。
	fresh, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	assertRoundCandidatesSorted(t, fresh)
	assertCandidateState(t, fresh, local.BlockID, true, CandidatePending, localIDs,
		pubkeySet(keys[0].pub), twoUnvoted)
	assertCandidateState(t, fresh, alt.BlockID, false, CandidatePending, altIDs,
		pubkeySet(keys[1].pub), twoUnvoted)

	// 本轮未结束：编辑查询结果不能伪造投票。真实验证者重复投原候选仍不增票，
	// 改投仍被拒绝，原选择保留（两个候选各一票、两人未投票）。
	if res, err := n.Vote(keys[0].pub, 1, local.BlockID); err != nil || res.Counted {
		t.Fatalf("duplicate local vote must not count: %+v %v", res, err)
	}
	if _, err := n.Vote(keys[0].pub, 1, alt.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switch vote got %v, want %s", err, ReasonAlreadyVoted)
	}
	if res, err := n.Vote(keys[1].pub, 1, alt.BlockID); err != nil || res.Counted {
		t.Fatalf("duplicate alt vote must not count: %+v %v", res, err)
	}
	if _, err := n.Vote(keys[1].pub, 1, local.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switch vote got %v, want %s", err, ReasonAlreadyVoted)
	}
	afterReject, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateState(t, afterReject, local.BlockID, true, CandidatePending, localIDs,
		pubkeySet(keys[0].pub), twoUnvoted)
	assertCandidateState(t, afterReject, alt.BlockID, false, CandidatePending, altIDs,
		pubkeySet(keys[1].pub), twoUnvoted)

	// 让剩余两名验证者继续投给竞争候选：第二张竞争候选票仍不能确认，
	// 第三张应立即确认并进入下一轮。
	r2, err := n.Vote(keys[2].pub, 1, alt.BlockID)
	if err != nil || !r2.Counted || r2.Confirmed {
		t.Fatalf("second alt vote = %+v %v, want counted without confirm", r2, err)
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round=%d height=%d, must stay 1/0 after 2 alt votes", n.CurrentRound(), n.Height())
	}
	// 决定性投票发生在调用方篡改之后，其落盘内容必须仍是节点的真实记录。
	r3, err := n.Vote(keys[3].pub, 1, alt.BlockID)
	if err != nil || !r3.Counted || !r3.Confirmed || r3.Block == nil {
		t.Fatalf("third alt vote = %+v %v, want immediate confirmation", r3, err)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	assertBlock(t, *r3.Block, altIDs, 1, 1, alt.BlockID, "")

	// 新查询旧轮次：竞争候选 won、本地提议 lost，各候选保留实际投票者，未投票为空。
	ended, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !ended.Ended || ended.UnconfirmedEnd {
		t.Fatalf("decided round record wrong: %+v", ended)
	}
	assertRoundCandidatesSorted(t, ended)
	assertCandidateState(t, ended, alt.BlockID, false, CandidateWon, altIDs,
		pubkeySet(keys[1].pub, keys[2].pub, keys[3].pub), pubkeySet())
	assertCandidateState(t, ended, local.BlockID, true, CandidateLost, localIDs,
		pubkeySet(keys[0].pub), pubkeySet())

	// 先前保留的未编辑结果仍显示当时双方各一票、两个候选未决、两人未投票，
	// 不随节点变化而更新（saved 自查询当时深拷贝后从未编辑）。
	if saved.Round != 1 || saved.Ended {
		t.Fatalf("saved snapshot header changed: %+v", saved)
	}
	assertRoundCandidatesSorted(t, saved)
	assertCandidateState(t, saved, local.BlockID, true, CandidatePending, localIDs,
		pubkeySet(keys[0].pub), twoUnvoted)
	assertCandidateState(t, saved, alt.BlockID, false, CandidatePending, altIDs,
		pubkeySet(keys[1].pub), twoUnvoted)

	// 篡改发生在确认保存之前：重开状态目录，历史仍是真实的胜出结果，
	// 伪造标识没有作为候选或交易落盘。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CurrentRound() != 2 || reopened.Height() != 1 {
		t.Fatalf("reopened round=%d height=%d, want 2/1", reopened.CurrentRound(), reopened.Height())
	}
	storedBlock, err := reopened.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	assertBlock(t, storedBlock, altIDs, 1, 1, alt.BlockID, "")
	storedRound, err := reopened.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateState(t, storedRound, alt.BlockID, false, CandidateWon, altIDs,
		pubkeySet(keys[1].pub, keys[2].pub, keys[3].pub), pubkeySet())
	assertCandidateState(t, storedRound, local.BlockID, true, CandidateLost, localIDs,
		pubkeySet(keys[0].pub), pubkeySet())
	if candidateByBlockID(storedRound, fakeBlockID+"-0") != nil ||
		candidateByBlockID(storedRound, fakeBlockID+"-1") != nil {
		t.Fatal("fake block id must not persist as candidate")
	}
	if _, err := reopened.Tx(fakeTxID); reason(err) != ReasonUnknownTx {
		t.Fatalf("fake tx id persisted: %v", err)
	}
}

// 空交易列表的候选：返回结果中自行添加交易不能登记出新内容；
// 尚无投票的候选：在结果中追加投票者公钥不能伪造得票，缩短未投票名单也不
// 缩减节点真实的未投票名单。
func TestCandidatesQueryEmptyListAndNoVotesIndependent(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	tx := NewTransaction(keys[0].priv, 1, []byte("only"), 5, 100)
	if _, err := n.Submit(tx); err != nil {
		t.Fatal(err)
	}
	local, _ := n.Propose()
	// 空交易列表的竞争候选；此时两个候选都尚无投票。
	emptyAlt, err := n.RegisterCandidate(1, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(local.TxIDs) != 1 || local.TxIDs[0] != tx.ID() {
		t.Fatalf("setup: local = %v, want [%s]", local.TxIDs, tx.ID())
	}

	rc := mustCandidates(t, n, 1)
	assertRoundCandidatesSorted(t, rc)
	allValidators := pubkeySet(keys[0].pub, keys[1].pub, keys[2].pub, keys[3].pub)
	assertCandidateState(t, rc, local.BlockID, true, CandidatePending, []string{tx.ID()},
		pubkeySet(), allValidators)
	assertCandidateState(t, rc, emptyAlt.BlockID, false, CandidatePending, []string{},
		pubkeySet(), allValidators)

	// 调用方向手中的空列表添加交易、向零票候选追加投票者、自行缩短未投票名单。
	var emptyView, localView *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case emptyAlt.BlockID:
			emptyView = &rc.Candidates[i]
		case local.BlockID:
			localView = &rc.Candidates[i]
		}
	}
	if emptyView == nil || localView == nil {
		t.Fatalf("candidate views missing: %+v", rc.Candidates)
	}
	emptyView.TxIDs = append(emptyView.TxIDs, tx.ID(), fakeTxID)
	emptyView.Voters = append(emptyView.Voters, bytes.Repeat([]byte{0x55}, 32))
	localView.Voters = append(localView.Voters, bytes.Repeat([]byte{0x66}, 32))
	rc.Unvoted = rc.Unvoted[:2]

	// 再次查询：空候选仍是空列表、零票；本地候选仍零票；未投票仍是全部四人。
	fresh := mustCandidates(t, n, 1)
	assertRoundCandidatesSorted(t, fresh)
	assertCandidateState(t, fresh, local.BlockID, true, CandidatePending, []string{tx.ID()},
		pubkeySet(), allValidators)
	assertCandidateState(t, fresh, emptyAlt.BlockID, false, CandidatePending, []string{},
		pubkeySet(), allValidators)

	// 多份结果之间同样独立：改写另一份，先保留的那份仍是查询当时的零票空列表。
	held := mustCandidates(t, n, 1)
	fresh.Candidates[0].TxIDs = append(fresh.Candidates[0].TxIDs, fakeTxID)
	fresh.Candidates[0].Voters = append(fresh.Candidates[0].Voters, bytes.Repeat([]byte{0x77}, 32))
	fresh.Unvoted = nil
	assertCandidateState(t, held, emptyAlt.BlockID, false, CandidatePending, []string{},
		pubkeySet(), allValidators)

	// 伪造标识没有借助调用方手中的列表登记成交易或候选，也不能凭伪造标识投票。
	if _, err := n.Tx(fakeTxID); reason(err) != ReasonUnknownTx {
		t.Fatalf("fake tx lookup got %v, want %s", err, ReasonUnknownTx)
	}
	if candidateByBlockID(mustCandidates(t, n, 1), fakeBlockID) != nil {
		t.Fatal("fake block id must not be a candidate")
	}
	if _, err := n.Vote(keys[0].pub, 1, fakeBlockID); reason(err) != ReasonWrongBlock {
		t.Fatalf("vote for fake block id got %v, want %s", err, ReasonWrongBlock)
	}
	rc2 := mustCandidates(t, n, 1)
	assertCandidateState(t, rc2, local.BlockID, true, CandidatePending, []string{tx.ID()},
		pubkeySet(), allValidators)
	assertCandidateState(t, rc2, emptyAlt.BlockID, false, CandidatePending, []string{},
		pubkeySet(), allValidators)
}

// 连续多份候选查询彼此独立，且是查询当时的快照：节点随后收到合法投票、
// 确认并进入下一轮，已取得的结果保留各自取得时的内容（票数、结果、未投票），
// 新查询反映节点的最新真实记录。
func TestCandidatesQueryMultipleSnapshotsAcrossVotes(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	tx := NewTransaction(keys[0].priv, 1, []byte("snap"), 5, 100)
	if _, err := n.Submit(tx); err != nil {
		t.Fatal(err)
	}
	local, _ := n.Propose()
	txIDs := []string{tx.ID()}
	all := pubkeySet(keys[0].pub, keys[1].pub, keys[2].pub, keys[3].pub)

	// 尚无投票时取得第一份。
	s0 := mustCandidates(t, n, 1)
	snap0 := snapshotRoundCandidates(s0)
	assertCandidateState(t, s0, local.BlockID, true, CandidatePending, txIDs, pubkeySet(), all)

	// 一票后取得第二份。
	if res, err := n.Vote(keys[0].pub, 1, local.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("vote 1 = %+v %v", res, err)
	}
	s1 := mustCandidates(t, n, 1)
	snap1 := snapshotRoundCandidates(s1)
	assertCandidateState(t, s1, local.BlockID, true, CandidatePending, txIDs,
		pubkeySet(keys[0].pub), pubkeySet(keys[1].pub, keys[2].pub, keys[3].pub))

	// 两票后取得第三份并随即改写第二份。
	if _, err := n.Vote(keys[1].pub, 1, local.BlockID); err != nil {
		t.Fatal(err)
	}
	s2 := mustCandidates(t, n, 1)
	tamperRoundCandidates(s1)

	// 第一份仍是零票；第二份的原值仍保留在 snap1；第三份是两票。
	assertSnapshotsEqual(t, s0, snap0)
	assertCandidateState(t, s2, local.BlockID, true, CandidatePending, txIDs,
		pubkeySet(keys[0].pub, keys[1].pub), pubkeySet(keys[2].pub, keys[3].pub))
	assertPubkeySet(t, snap0.Unvoted, all, "snap0 unvoted")
	c1 := candidateByBlockID(snap1, local.BlockID)
	if c1 == nil || c1.Result != CandidatePending || len(c1.Voters) != 1 {
		t.Fatalf("snap1 candidate should keep its 1 pending vote: %+v", c1)
	}
	assertPubkeySet(t, snap1.Unvoted, pubkeySet(keys[1].pub, keys[2].pub, keys[3].pub), "snap1 unvoted")

	// 第三票确认进入下一轮。
	if res, err := n.Vote(keys[2].pub, 1, local.BlockID); err != nil || !res.Confirmed {
		t.Fatalf("vote 3 = %+v %v, want confirmation", res, err)
	}
	if n.CurrentRound() != 2 {
		t.Fatalf("round = %d, want 2", n.CurrentRound())
	}

	// 旧结果不随节点变化更新。
	assertSnapshotsEqual(t, s0, snap0)
	assertPubkeySet(t, snap0.Unvoted, all, "snap0 unvoted after confirm")
	c0 := candidateByBlockID(snap0, local.BlockID)
	if c0 == nil || c0.Result != CandidatePending || len(c0.Voters) != 0 {
		t.Fatalf("snap0 candidate changed after confirm: %+v", c0)
	}
	c1 = candidateByBlockID(snap1, local.BlockID)
	if c1 == nil || c1.Result != CandidatePending || len(c1.Voters) != 1 {
		t.Fatalf("snap1 candidate changed after confirm: %+v", c1)
	}
	assertPubkeySet(t, snap1.Unvoted, pubkeySet(keys[1].pub, keys[2].pub, keys[3].pub), "snap1 unvoted after confirm")

	// 新查询旧轮反映真实结果：胜出、三名投票者；keys[3] 未投票仍列入名单。
	ended := mustCandidates(t, n, 1)
	assertCandidateState(t, ended, local.BlockID, true, CandidateWon, txIDs,
		pubkeySet(keys[0].pub, keys[1].pub, keys[2].pub), pubkeySet(keys[3].pub))
}
