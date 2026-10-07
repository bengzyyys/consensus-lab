// 示例：账户序号缺口怎样限制跨账户的费用排序——费用最高的交易为什么仍在等待打包。
//
// 流程：两名发送账户、四名验证者、单块上限两笔、交易池不限容量。
// 先交甲账户序号 2、费用 100 与乙账户序号 1、费用 20，在产生本地提议前查询甲账户：
// 已确认序号 0、待处理只有甲 2、最早缺口为 1，说明 waiting-pack。随后补交甲账户
// 序号 1、费用 5，缺口归零、待处理按序号 1、2 排列。本轮本地提议顺序为 乙1、甲1：
// 甲1 入选前甲2 不能越过它参与费用竞争，甲1 入选后本块已满，费用最高的甲2 继续
// queued、说明仍是 waiting-pack。三名名单内验证者投票确认后，甲已确认序号变为 1、
// 待处理只剩甲2 且缺口为 0；下一轮本地提议只包含甲2，说明变为 waiting-vote，
// 但甲已确认序号仍为 1——选入提议只是等待投票，并不等于确认。
//
// 运行：go run ./examples/sequence-gap-ordering
package main

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"log"
	"os"

	"github.com/bengzyyys/consensus-lab/consensus"
)

// check 处理正常调用的错误：提交、查询、提议或投票一旦失败就明确终止演示，
// 不会继续输出任何“确认成功”。
func check(err error) {
	if err != nil {
		log.Fatalf("unexpected error: %v", err)
	}
}

// demoKey 由固定字节派生演示密钥，使示例输出可复现；
// 真实应用应使用 crypto/rand 随机生成并妥善保管私钥。
func demoKey(b byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
}

func main() {
	log.SetFlags(0)

	// 四名验证者与两个互不相同的发送账户（甲、乙）。
	validatorKeys := make([]ed25519.PrivateKey, 4)
	validators := make([][]byte, 4)
	for i := range validatorKeys {
		validatorKeys[i] = demoKey(byte(i + 1))
		validators[i] = append([]byte(nil), validatorKeys[i].Public().(ed25519.PublicKey)...)
	}
	jiaKey := demoKey(0xa1)
	yiKey := demoKey(0xb0)
	jiaPub := jiaKey.Public().(ed25519.PublicKey)
	yiPub := yiKey.Public().(ed25519.PublicKey)
	names := map[string]string{
		fmt.Sprintf("%x", jiaPub): "甲",
		fmt.Sprintf("%x", yiPub):  "乙",
	}
	nameOf := func(sender []byte) string {
		return names[fmt.Sprintf("%x", sender)]
	}

	// 每次运行都在新的临时状态目录建立节点，程序退出时删除；
	// 固定种子与固定演示密钥使交易、区块标识等输出可复现。
	// PoolCapacity 省略即为 0：交易池不限制容量（与单块上限两笔相互独立）。
	dir, err := os.MkdirTemp("", "consensus-demo-*")
	check(err)
	defer os.RemoveAll(dir)

	n, err := consensus.New(dir, consensus.Config{
		Seed:           []byte("sequence-gap-ordering-demo"),
		Validators:     validators,
		MaxTxsPerBlock: 2,
	})
	check(err)
	fmt.Printf("节点已建立：轮次 %d，单块上限 2，验证者 4 名（确认需严格超过 2/3，即至少 3 票），交易池容量=%d（0 表示不限制；状态保存在新的临时目录，退出时删除）\n",
		n.CurrentRound(), n.Config().PoolCapacity)

	// mustTx 按标识查询交易，查询失败立即终止；打印时同时标明所属账户、序号、费用，
	// 方便与账户查询逐笔对照。
	mustTx := func(id string) *consensus.TxInfo {
		info, err := n.Tx(id)
		check(err)
		return info
	}
	printTx := func(label, id string) {
		info := mustTx(id)
		fmt.Printf("%s%s：账户=%s，序号=%d，费用=%d，状态=%s\n",
			label, id, nameOf(info.Tx.Sender), info.Tx.Sequence, info.Tx.Fee, info.Status)
	}
	// printAccount 打印账户查询结果并返回它，供随后断言；任何待处理交易都标明
	// 账户、序号、费用、状态与等待说明（waiting-pack / waiting-vote）。
	printAccount := func(name string, pub []byte) *consensus.AccountInfo {
		acct := n.Account(pub)
		fmt.Printf("账户%s：已确认序号=%d，待处理交易=%d 笔，最早缺口序号=%d\n",
			name, acct.ConfirmedSequence, len(acct.Pending), acct.Gap)
		for _, p := range acct.Pending {
			fmt.Printf("  待处理 %s：账户=%s，序号=%d，费用=%d，状态=%s，说明=%s\n",
				p.ID, nameOf(p.Tx.Sender), p.Tx.Sequence, p.Tx.Fee, p.Status, p.Note)
		}
		return acct
	}

	// 先交甲账户序号 2、费用 100（序号 1 尚未提交），再交乙账户序号 1、费用 20。
	// 两笔交易签名有效、到期轮次 100 在演示期间不会到达。
	txJia2 := consensus.NewTransaction(jiaKey, 2, []byte("jia: seq 2 fee 100"), 100, 100)
	txYi1 := consensus.NewTransaction(yiKey, 1, []byte("yi: seq 1 fee 20"), 20, 100)
	resJia2, err := n.Submit(txJia2)
	check(err)
	resYi1, err := n.Submit(txYi1)
	check(err)
	jia2 := resJia2.TxID
	yi1 := resYi1.TxID
	fmt.Println()
	printTx("已提交（甲，序号 2，费用 100）：", jia2)
	printTx("已提交（乙，序号 1，费用 20）：", yi1)

	// 产生本地提议之前查询甲账户：序号 1 缺失，序号 2 虽已入池却不能参与打包，
	// 账户查询把最早缺口指为 1，甲2 的说明是 waiting-pack（等待打包）。
	fmt.Println("\n—— 产生本地提议前查询甲账户 ——")
	acct := printAccount("甲", jiaPub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 1 || len(acct.Pending) != 1 {
		log.Fatalf("补交前甲账户查询异常：%+v", acct)
	}
	if acct.Pending[0].ID != jia2 || acct.Pending[0].Tx.Sequence != 2 ||
		acct.Pending[0].Status != consensus.StatusQueued || acct.Pending[0].Note != "waiting-pack" {
		log.Fatalf("甲2 应在池中等待打包，实际：%+v", acct.Pending[0])
	}

	// 补交甲账户序号 1、费用 5：缺口归零，待处理按序号 1、2 升序排列，均等待打包。
	txJia1 := consensus.NewTransaction(jiaKey, 1, []byte("jia: seq 1 fee 5"), 5, 100)
	resJia1, err := n.Submit(txJia1)
	check(err)
	jia1 := resJia1.TxID
	fmt.Println()
	printTx("补交（甲，序号 1，费用 5）：", jia1)
	fmt.Println("\n—— 补交后查询甲账户 ——")
	acct = printAccount("甲", jiaPub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 0 || len(acct.Pending) != 2 {
		log.Fatalf("补交后甲账户查询异常：%+v", acct)
	}
	if acct.Pending[0].Tx.Sequence != 1 || acct.Pending[0].ID != jia1 ||
		acct.Pending[1].Tx.Sequence != 2 || acct.Pending[1].ID != jia2 {
		log.Fatalf("待处理应按序号 1、2 排列，实际：%+v", acct.Pending)
	}

	// 本轮本地提议：打包规则每轮只在各账户“下一条可确认”的交易之间比费用。
	// 第一轮可选的是乙1（费用 20）与甲1（费用 5），甲2 因甲1 尚未入选而不能参与；
	// 乙1 入选后，甲1 才具备资格并占用第二个名额。故顺序为 乙1、甲1，
	// 费用最高的甲2 不在本块中。
	proposal, err := n.Propose()
	check(err)
	fmt.Printf("\n本地提议（轮次 %d）：区块 %s\n", proposal.Round, proposal.BlockID)
	for i, id := range proposal.TxIDs {
		info := mustTx(id)
		fmt.Printf("  打包顺序 %d：%s（账户=%s，序号=%d，费用=%d）\n",
			i+1, id, nameOf(info.Tx.Sender), info.Tx.Sequence, info.Tx.Fee)
	}
	wantOrder := []string{yi1, jia1}
	if len(proposal.TxIDs) != 2 || proposal.TxIDs[0] != wantOrder[0] || proposal.TxIDs[1] != wantOrder[1] {
		log.Fatalf("本地提议顺序应为 乙1、甲1，实际：%v", proposal.TxIDs)
	}
	for _, id := range proposal.TxIDs {
		if id == jia2 {
			log.Fatal("费用最高的甲2 不应进入本块")
		}
	}
	round1 := proposal.Round
	block1 := proposal.BlockID

	// 提议后逐笔查询：乙1、甲1 已进入提议等待投票；甲2 仍是 queued。
	fmt.Println("\n—— 提议后查询 ——")
	printTx("交易 ", yi1)
	printTx("交易 ", jia1)
	printTx("交易 ", jia2)
	if mustTx(jia2).Status != consensus.StatusQueued {
		log.Fatal("甲2 应仍在排队等待打包")
	}
	acct = printAccount("甲", jiaPub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 0 {
		log.Fatalf("提议不推进序号，甲账户异常：%+v", acct)
	}
	if len(acct.Pending) != 2 ||
		acct.Pending[0].ID != jia1 || acct.Pending[0].Status != consensus.StatusProposed || acct.Pending[0].Note != "waiting-vote" ||
		acct.Pending[1].ID != jia2 || acct.Pending[1].Status != consensus.StatusQueued || acct.Pending[1].Note != "waiting-pack" {
		log.Fatalf("甲1 应 waiting-vote、甲2 应仍 waiting-pack，实际：%+v", acct.Pending)
	}
	fmt.Println("缺口=0 只表示当前待处理序号连续，并不保证它们全部进入本块；")
	fmt.Println("甲2 虽费用最高：甲1 入选前它不能抢先参与费用竞争，甲1 入选后本块已满额，故继续排队。")

	// 名单内三名不同验证者投票确认这份本地提议：四人需严格超过 2/3，即第 3 票才确认。
	fmt.Println("\n—— 三名验证者依次投票确认轮次 1 的本地提议 ——")
	cast := func(idx int) *consensus.VoteResult {
		vr, err := n.Vote(validators[idx], round1, block1)
		check(err)
		if !vr.Counted {
			log.Fatalf("验证者 %d 的票应新计入", idx)
		}
		return vr
	}
	vr := cast(0)
	fmt.Printf("验证者 0 投票：计入=%v，确认=%v\n", vr.Counted, vr.Confirmed)
	if vr.Confirmed {
		log.Fatal("第 1 票不应确认")
	}
	vr = cast(1)
	fmt.Printf("验证者 1 投票：计入=%v，确认=%v（2 票未达 3 票门槛）\n", vr.Counted, vr.Confirmed)
	if vr.Confirmed {
		log.Fatal("第 2 票不应确认")
	}
	vr = cast(2)
	fmt.Printf("验证者 2 投票：计入=%v，确认=%v，新区块高度 %d\n", vr.Counted, vr.Confirmed, vr.Block.Height)
	if !vr.Confirmed || vr.Block == nil || vr.Block.Height != 1 || vr.Block.ID != block1 {
		log.Fatalf("第 3 票应确认本地提议为高度 1 的区块，实际：%+v", vr)
	}

	// 确认后查询：乙1、甲1 随区块确认；甲已确认序号推进为 1，待处理只剩甲2，缺口仍为 0。
	fmt.Println("\n—— 确认后查询 ——")
	printTx("交易 ", yi1)
	printTx("交易 ", jia1)
	printTx("交易 ", jia2)
	for _, id := range []string{yi1, jia1} {
		info := mustTx(id)
		if info.Status != consensus.StatusConfirmed || info.BlockHeight != 1 || info.BlockID != block1 {
			log.Fatalf("%s 应在高度 1、区块 %s 中确认，实际：%+v", id, block1, info)
		}
	}
	if mustTx(jia2).Status != consensus.StatusQueued {
		log.Fatal("甲2 未被确认，应继续排队")
	}
	acct = printAccount("甲", jiaPub)
	if acct.ConfirmedSequence != 1 || acct.Gap != 0 || len(acct.Pending) != 1 ||
		acct.Pending[0].ID != jia2 || acct.Pending[0].Status != consensus.StatusQueued ||
		acct.Pending[0].Note != "waiting-pack" {
		log.Fatalf("确认后甲账户应只剩甲2 等待打包，实际：%+v", acct)
	}
	printAccount("乙", yiPub)

	// 进入下一轮：本地提议只包含甲2。它进入候选后说明变为 waiting-vote，
	// 但甲已确认序号仍是 1——选入提议只是等待投票，不等于确认。
	proposal2, err := n.Propose()
	check(err)
	fmt.Printf("\n下一轮本地提议（轮次 %d）：区块 %s\n", proposal2.Round, proposal2.BlockID)
	for i, id := range proposal2.TxIDs {
		info := mustTx(id)
		fmt.Printf("  打包顺序 %d：%s（账户=%s，序号=%d，费用=%d）\n",
			i+1, id, nameOf(info.Tx.Sender), info.Tx.Sequence, info.Tx.Fee)
	}
	if proposal2.Round != 2 || len(proposal2.TxIDs) != 1 || proposal2.TxIDs[0] != jia2 {
		log.Fatalf("轮次 2 提议应只包含甲2，实际：%+v", proposal2)
	}
	fmt.Println("\n—— 第二轮提议后再次查询 ——")
	printTx("交易 ", jia2)
	acct = printAccount("甲", jiaPub)
	if acct.ConfirmedSequence != 1 {
		log.Fatalf("进入提议不会推进序号，甲已确认序号应为 1，实际：%d", acct.ConfirmedSequence)
	}
	if len(acct.Pending) != 1 || acct.Pending[0].ID != jia2 ||
		acct.Pending[0].Status != consensus.StatusProposed || acct.Pending[0].Note != "waiting-vote" {
		log.Fatalf("甲2 应已进入提议、说明 waiting-vote，实际：%+v", acct.Pending)
	}
	fmt.Printf("当前轮次=%d，确认高度=%d，甲已确认序号仍为 1：甲2 只是 waiting-vote，尚未确认。\n",
		n.CurrentRound(), n.Height())
}
