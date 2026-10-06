package consensus

import (
	"fmt"
	"testing"
)

// setupContendingRound 建立竞争候选场景：本地提议含 Alice（费用 20）与
// Bob（费用 10）两笔不同账户的交易，竞争候选只含 Alice 交易；四名验证者中
// keys[0] 投本地提议、keys[1] 投竞争候选，keys[2]、keys[3] 尚未投票。
// 返回节点、本地提议、竞争候选标识与两笔交易标识。
func setupContendingRound(t *testing.T) (n *Node, keys []testKey, local ProposalView, altID, aliceID, bobID string) {
	t.Helper()
	keys = genKeys(t, 4)
	n, _ = newTestNode(t, keys, 10)
	alice := NewTransaction(keys[0].priv, 1, []byte("alice"), 20, 100)
	bob := NewTransaction(keys[1].priv, 1, []byte("bob"), 10, 100)
	if _, err := n.Submit(alice); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(bob); err != nil {
		t.Fatal(err)
	}
	aliceID, bobID = alice.ID(), bob.ID()

	var err error
	local, err = n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{aliceID, bobID}) {
		t.Fatalf("setup: local proposal = %v, want [%s %s]", local.TxIDs, aliceID, bobID)
	}
	altID = registerMust(t, n, 1, []string{aliceID})
	if altID == local.BlockID {
		t.Fatal("setup: contending candidate must differ from local proposal")
	}
	if res, err := n.Vote(keys[0].pub, 1, local.BlockID); err != nil || !res.Counted {
		t.Fatalf("setup: vote for local: %+v %v", res, err)
	}
	if res, err := n.Vote(keys[1].pub, 1, altID); err != nil || !res.Counted {
		t.Fatalf("setup: vote for alt: %+v %v", res, err)
	}
	return n, keys, local, altID, aliceID, bobID
}

// pubHexList 返回公钥列表的十六进制形式，便于与期望值比较。
func pubHexList(pubs [][]byte) []string {
	out := make([]string, len(pubs))
	for i, p := range pubs {
		out[i] = fmt.Sprintf("%x", p)
	}
	return out
}

// sortedPubHex 返回按十六进制定序的公钥列表（节点返回投票者与未投票名单的顺序）。
func sortedPubHex(pubs ...[]byte) []string {
	out := pubHexList(pubs)
	sortStrings(out)
	return out
}

// candidateByID 在轮次查询结果中按区块标识找到候选视图，找不到则失败。
func candidateByID(t *testing.T, rc *RoundCandidates, blockID string) *CandidateView {
	t.Helper()
	for i := range rc.Candidates {
		if rc.Candidates[i].BlockID == blockID {
			return &rc.Candidates[i]
		}
	}
	t.Fatalf("candidate %s missing from %+v", blockID, rc.Candidates)
	return nil
}

// assertPendingContendingView 校验轮次查询结果正是投票后、决出前的真实记录：
// 两个候选按区块标识排序且均为 pending，各自保留真实交易顺序与投票者，
// keys[2]、keys[3] 按公钥序列入未投票名单，轮次未结束。
func assertPendingContendingView(t *testing.T, rc *RoundCandidates, keys []testKey, localID, altID, aliceID, bobID string) {
	t.Helper()
	if rc.Round != 1 || rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round view header wrong: %+v", rc)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2: %+v", len(rc.Candidates), rc.Candidates)
	}
	if rc.Candidates[0].BlockID > rc.Candidates[1].BlockID {
		t.Fatalf("candidates not sorted by block id: %s then %s",
			rc.Candidates[0].BlockID, rc.Candidates[1].BlockID)
	}
	local := candidateByID(t, rc, localID)
	if !local.Local || local.Result != CandidatePending || local.Round != 1 {
		t.Fatalf("local candidate wrong: %+v", local)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{aliceID, bobID}) {
		t.Fatalf("local tx order = %v, want [%s %s]", local.TxIDs, aliceID, bobID)
	}
	if fmt.Sprint(pubHexList(local.Voters)) != fmt.Sprint(sortedPubHex(keys[0].pub)) {
		t.Fatalf("local voters = %x, want keys[0]", local.Voters)
	}
	alt := candidateByID(t, rc, altID)
	if alt.Local || alt.Result != CandidatePending || alt.Round != 1 {
		t.Fatalf("alt candidate wrong: %+v", alt)
	}
	if fmt.Sprint(alt.TxIDs) != fmt.Sprint([]string{aliceID}) {
		t.Fatalf("alt tx order = %v, want [%s]", alt.TxIDs, aliceID)
	}
	if fmt.Sprint(pubHexList(alt.Voters)) != fmt.Sprint(sortedPubHex(keys[1].pub)) {
		t.Fatalf("alt voters = %x, want keys[1]", alt.Voters)
	}
	if fmt.Sprint(pubHexList(rc.Unvoted)) != fmt.Sprint(sortedPubHex(keys[2].pub, keys[3].pub)) {
		t.Fatalf("unvoted = %x, want keys[2] and keys[3] sorted", rc.Unvoted)
	}
}

// 同一轮存在本地提议与竞争候选、两者共享 Alice 交易且各有一票时，连续取得的
// 两份查询结果彼此独立：改写其中一份的候选标识、结果、交易列表、投票者公钥字节
// 与未投票名单，另一份保持取得时的内容，再次查询仍看到节点的真实记录；同一份
// 结果内共享交易的两个候选也各有独立的交易列表。本轮未结束时，编辑查询结果
// 不能伪造投票：重复投原候选不增票，改投被拒绝且原选择保留。
func TestCandidatesQueryResultsAreIndependentCopies(t *testing.T) {
	n, keys, local, altID, aliceID, bobID := setupContendingRound(t)
	localID := local.BlockID

	// 连续取得两份结果。
	first, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	assertPendingContendingView(t, first, keys, localID, altID, aliceID, bobID)

	// 调用方尽力改写第一份结果：轮次头、候选标识、结果、交易列表、
	// 投票者公钥字节与名单、未投票名单。
	first.Round = 99
	first.Ended = true
	first.UnconfirmedEnd = true
	firstLocal := candidateByID(t, first, localID)
	firstLocal.BlockID = "tampered-local-id"
	firstLocal.Result = CandidateWon
	firstLocal.TxIDs[0] = "tampered-tx"
	firstLocal.TxIDs = append(firstLocal.TxIDs, "forged-extra-tx")
	firstLocal.Voters[0][0] ^= 0xff
	firstLocal.Voters = append(firstLocal.Voters, keys[2].pub)
	firstAlt := candidateByID(t, first, altID)
	firstAlt.Result = CandidateLost
	firstAlt.TxIDs = nil
	first.Unvoted[0][0] ^= 0xff
	first.Unvoted = append(first.Unvoted, keys[0].pub)

	// 第二份结果保持取得时的内容。
	assertPendingContendingView(t, second, keys, localID, altID, aliceID, bobID)

	// 再次查询仍看到节点的真实记录。
	third, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	assertPendingContendingView(t, third, keys, localID, altID, aliceID, bobID)

	// 同一份结果内，共享 Alice 交易的两个候选各有独立列表：
	// 改写本地候选的交易列表，竞争候选的列表不变，反之亦然。
	secondLocal := candidateByID(t, second, localID)
	secondAlt := candidateByID(t, second, altID)
	secondLocal.TxIDs[0] = "tampered-shared-tx"
	if secondAlt.TxIDs[0] != aliceID {
		t.Fatalf("editing local tx list changed alt list: %v", secondAlt.TxIDs)
	}
	secondAlt.TxIDs[0] = "tampered-alt-tx"
	if secondLocal.TxIDs[1] != bobID {
		t.Fatalf("editing alt tx list changed local list: %v", secondLocal.TxIDs)
	}

	// 编辑查询结果不能伪造投票：真实验证者重复投原候选不增票。
	if res, err := n.Vote(keys[1].pub, 1, altID); err != nil || res.Counted {
		t.Fatalf("duplicate vote after result tamper: %+v %v", res, err)
	}
	if res, err := n.Vote(keys[0].pub, 1, localID); err != nil || res.Counted {
		t.Fatalf("duplicate local vote after result tamper: %+v %v", res, err)
	}
	// 改投仍被拒绝，原选择保留。
	if _, err := n.Vote(keys[1].pub, 1, localID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switch vote after result tamper got %v, want %s", err, ReasonAlreadyVoted)
	}
	if _, err := n.Vote(keys[0].pub, 1, altID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switch local vote after result tamper got %v, want %s", err, ReasonAlreadyVoted)
	}
	after, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	assertPendingContendingView(t, after, keys, localID, altID, aliceID, bobID)
}

// 查询结果是取得时的快照：保留未编辑的旧结果后，剩余两名验证者继续投给竞争
// 候选，第二票仍不能确认（严格超过 2/3 才确认，四人名单需 3 票），第三票立即
// 确认并进入下一轮。新查询旧轮次显示竞争候选胜出、本地提议落选、各候选保留
// 实际投票者、未投票名单为空；先前保留的结果仍显示当时双方各一票、两个候选
// 未决、两人未投票，不随节点变化而更新。
func TestCandidatesQuerySnapshotSurvivesConfirmation(t *testing.T) {
	n, keys, local, altID, aliceID, bobID := setupContendingRound(t)
	localID := local.BlockID

	// 保留一份未编辑的旧结果。
	saved, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	assertPendingContendingView(t, saved, keys, localID, altID, aliceID, bobID)

	// 第二票投给竞争候选：计入但仍未确认（2 票未严格超过 4 人的 2/3）。
	res, err := n.Vote(keys[2].pub, 1, altID)
	if err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("second alt vote: %+v %v", res, err)
	}
	if n.CurrentRound() != 1 {
		t.Fatalf("round advanced at 2 votes: %d", n.CurrentRound())
	}
	// 第三票立即确认并进入下一轮。
	res, err = n.Vote(keys[3].pub, 1, altID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("third alt vote should confirm: %+v %v", res, err)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("after confirm round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}

	// 新查询旧轮次：竞争候选胜出、本地提议落选，各候选保留实际投票者，
	// 未投票名单为空。
	decided, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !decided.Ended || decided.UnconfirmedEnd || len(decided.Unvoted) != 0 {
		t.Fatalf("decided round view wrong: %+v", decided)
	}
	if len(decided.Candidates) != 2 ||
		decided.Candidates[0].BlockID > decided.Candidates[1].BlockID {
		t.Fatalf("decided candidates wrong: %+v", decided.Candidates)
	}
	won := candidateByID(t, decided, altID)
	if won.Result != CandidateWon || won.Local {
		t.Fatalf("alt candidate should have won: %+v", won)
	}
	if fmt.Sprint(won.TxIDs) != fmt.Sprint([]string{aliceID}) {
		t.Fatalf("won tx order = %v, want [%s]", won.TxIDs, aliceID)
	}
	if fmt.Sprint(pubHexList(won.Voters)) != fmt.Sprint(sortedPubHex(keys[1].pub, keys[2].pub, keys[3].pub)) {
		t.Fatalf("won voters = %x, want keys[1..3] sorted", won.Voters)
	}
	lost := candidateByID(t, decided, localID)
	if lost.Result != CandidateLost || !lost.Local {
		t.Fatalf("local candidate should have lost: %+v", lost)
	}
	if fmt.Sprint(lost.TxIDs) != fmt.Sprint([]string{aliceID, bobID}) {
		t.Fatalf("lost tx order = %v, want [%s %s]", lost.TxIDs, aliceID, bobID)
	}
	if fmt.Sprint(pubHexList(lost.Voters)) != fmt.Sprint(sortedPubHex(keys[0].pub)) {
		t.Fatalf("lost voters = %x, want keys[0]", lost.Voters)
	}

	// 先前保留的结果仍显示取得时的内容：双方各一票、两个候选未决、两人未投票。
	assertPendingContendingView(t, saved, keys, localID, altID, aliceID, bobID)
}

// 空交易列表的候选与尚无投票的候选遵守相同的独立副本约定：调用方在返回结果中
// 自行添加交易或投票者，不会在节点中登记出新内容；候选按区块标识排序、公钥按
// 字典序排列的返回行为保持不变。
func TestCandidatesQueryEmptyAndVotelessCandidateCopy(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	alice := NewTransaction(keys[0].priv, 1, []byte("alice"), 5, 100)
	if _, err := n.Submit(alice); err != nil {
		t.Fatal(err)
	}
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	emptyID := registerMust(t, n, 1, nil) // 空交易列表的竞争候选
	if emptyID != BlockID(1, 1, "", []string{}) {
		t.Fatalf("empty candidate id = %s, want recomputed %s", emptyID, BlockID(1, 1, "", []string{}))
	}
	// 只有本地提议得到一票；空候选既无交易也无投票。
	if res, err := n.Vote(keys[0].pub, 1, local.BlockID); err != nil || !res.Counted {
		t.Fatalf("setup: vote for local: %+v %v", res, err)
	}

	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 2 || rc.Candidates[0].BlockID > rc.Candidates[1].BlockID {
		t.Fatalf("candidates not sorted by block id: %+v", rc.Candidates)
	}
	empty := candidateByID(t, rc, emptyID)
	if len(empty.TxIDs) != 0 || len(empty.Voters) != 0 || empty.Result != CandidatePending {
		t.Fatalf("setup: empty candidate wrong: %+v", empty)
	}
	if fmt.Sprint(pubHexList(rc.Unvoted)) != fmt.Sprint(sortedPubHex(keys[1].pub, keys[2].pub, keys[3].pub)) {
		t.Fatalf("setup: unvoted = %x, want keys[1..3] sorted", rc.Unvoted)
	}

	// 调用方在手中的结果里为空候选添加交易与投票者，并改写本地候选的列表。
	empty.TxIDs = append(empty.TxIDs, alice.ID(), "forged-tx")
	empty.Voters = append(empty.Voters, keys[1].pub)
	empty.Result = CandidateWon
	localView := candidateByID(t, rc, local.BlockID)
	localView.TxIDs[0] = "tampered-tx"
	localView.Voters[0][0] ^= 0xff
	rc.Unvoted[0][0] ^= 0xff

	// 再次查询：空候选仍无交易、无投票者，本地候选保持真实记录。
	again, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Candidates) != 2 {
		t.Fatalf("caller edits registered new candidates: %+v", again.Candidates)
	}
	emptyAgain := candidateByID(t, again, emptyID)
	if len(emptyAgain.TxIDs) != 0 || len(emptyAgain.Voters) != 0 || emptyAgain.Result != CandidatePending {
		t.Fatalf("caller edits leaked into empty candidate: %+v", emptyAgain)
	}
	localAgain := candidateByID(t, again, local.BlockID)
	if fmt.Sprint(localAgain.TxIDs) != fmt.Sprint([]string{alice.ID()}) {
		t.Fatalf("local tx list changed by caller edit: %v", localAgain.TxIDs)
	}
	if fmt.Sprint(pubHexList(localAgain.Voters)) != fmt.Sprint(sortedPubHex(keys[0].pub)) {
		t.Fatalf("local voters changed by caller edit: %x", localAgain.Voters)
	}
	if fmt.Sprint(pubHexList(again.Unvoted)) != fmt.Sprint(sortedPubHex(keys[1].pub, keys[2].pub, keys[3].pub)) {
		t.Fatalf("unvoted changed by caller edit: %x", again.Unvoted)
	}

	// 节点没有登记出伪造的投票：keys[1] 投给空候选仍是第一票、正常计入。
	res, err := n.Vote(keys[1].pub, 1, emptyID)
	if err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("first real vote for empty candidate: %+v %v", res, err)
	}
	final, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	emptyFinal := candidateByID(t, final, emptyID)
	if fmt.Sprint(pubHexList(emptyFinal.Voters)) != fmt.Sprint(sortedPubHex(keys[1].pub)) {
		t.Fatalf("empty candidate voters = %x, want exactly keys[1]", emptyFinal.Voters)
	}
	if len(emptyFinal.TxIDs) != 0 {
		t.Fatalf("empty candidate gained txs: %v", emptyFinal.TxIDs)
	}
}
