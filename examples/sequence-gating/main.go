// 示例：账户序号怎样限制跨账户的费用排序——费用最高的甲 2 在甲 1 入池前没有
// 参选资格，甲 1 入选后本块又已满额，因此甲 2 继续排队，直到下一轮才进入提议。
//
// 流程：甲提交序号 2（费用 100）、乙提交序号 1（费用 20）-> 产生本地提议前查询
// 甲账户：已确认序号 0、最早缺口 1、说明 waiting-pack -> 补交甲序号 1（费用 5），
// 缺口变 0、待处理按序号 1、2 排列 -> 本地提议顺序为乙 1、甲 1，费用最高的甲 2
// 仍为 queued（waiting-pack）-> 名单内三名验证者投票确认 -> 甲已确认序号变为 1，
// 待处理只剩甲 2、缺口仍为 0 -> 下一轮本地提议只含甲 2，说明变 waiting-vote，
// 但甲已确认序号仍为 1（进入提议不等于得到确认）。
//
// 运行：go run ./examples/sequence-gating
package main

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"log"
	"os"

	"github.com/bengzyyys/consensus-lab/consensus"
)

// check 处理正常调用的错误：任何非预期失败都直接终止演示，
// 不会继续输出确认成功之类的结果。
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

// short 截断长标识便于对照阅读；标识本身均来自实际返回值。
func short(id string) string {
	if len(id) > 16 {
		return id[:16] + "…"
	}
	return id
}

// printPending 打印一条账户待处理交易；所属账户、序号与费用均取自
// 查询返回的交易内容，方便与提交时的输入逐笔对照。
func printPending(p *consensus.TxInfo, senderName map[string]string) {
	fmt.Printf("  待处理 %s：账户=%s，序号=%d，费用=%d，状态=%s，说明=%s\n",
		short(p.ID), senderName[p.Tx.SenderHex()], p.Tx.Sequence, p.Tx.Fee, p.Status, p.Note)
}

// printAccount 打印一次账户查询的完整结果。
func printAccount(acct *consensus.AccountInfo, senderName map[string]string) {
	fmt.Printf("已确认序号=%d，待处理交易=%d 笔，最早缺口=%d\n",
		acct.ConfirmedSequence, len(acct.Pending), acct.Gap)
	for _, p := range acct.Pending {
		printPending(p, senderName)
	}
}

func main() {
	log.SetFlags(0)

	// 四名验证者与两个发送账户（甲、乙）。
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
	senderName := map[string]string{
		fmt.Sprintf("%x", jiaPub): "甲",
		fmt.Sprintf("%x", yiPub):  "乙",
	}

	// 每次运行都在新的临时状态目录建立节点，程序退出时删除；
	// 固定种子与固定演示密钥使交易、区块标识等输出可复现。
	// 单块上限两笔，交易池容量不限制（PoolCapacity 省略即 0）。
	dir, err := os.MkdirTemp("", "consensus-demo-*")
	check(err)
	defer os.RemoveAll(dir)

	n, err := consensus.New(dir, consensus.Config{
		Seed:           []byte("sequence-gating-demo"),
		Validators:     validators,
		MaxTxsPerBlock: 2,
	})
	check(err)
	fmt.Printf("节点已建立：轮次 %d，单块上限 2，交易池容量不限，验证者 4 名"+
		"（确认需严格超过 2/3，即至少 3 票；状态保存在新的临时目录，退出时删除）\n",
		n.CurrentRound())

	// 先提交甲序号 2（费用 100，全场最高）与乙序号 1（费用 20）。
	// 甲的已确认序号为 0，序号 1 尚未入池，序号 2 可以入池等待（允许缺口）。
	txJia2 := consensus.NewTransaction(jiaKey, 2, []byte("jia: seq 2, fee 100"), 100, 100)
	res, err := n.Submit(txJia2)
	check(err)
	jia2 := res.TxID
	fmt.Printf("\n已提交 甲 序号 2（费用 100，到期轮次 100）：%s\n", jia2)

	txYi1 := consensus.NewTransaction(yiKey, 1, []byte("yi: seq 1, fee 20"), 20, 100)
	res, err = n.Submit(txYi1)
	check(err)
	yi1 := res.TxID
	fmt.Printf("已提交 乙 序号 1（费用 20，到期轮次 100）：%s\n", yi1)

	// 产生本地提议前查询甲账户：已确认序号 0，待处理只有甲 2，
	// 最早缺口为 1，说明 waiting-pack——序号 1 未到，甲 2 只能在池内等待。
	acct := n.Account(jiaPub)
	if acct.ConfirmedSequence != 0 || len(acct.Pending) != 1 || acct.Gap != 1 ||
		acct.Pending[0].ID != jia2 || acct.Pending[0].Note != "waiting-pack" {
		log.Fatalf("提议前甲账户状态异常：%+v", acct)
	}
	fmt.Println("\n—— 产生本地提议前查询甲账户 ——")
	printAccount(acct, senderName)

	// 补交甲序号 1（费用 5）：缺口变为 0，待处理按序号 1、2 排列。
	txJia1 := consensus.NewTransaction(jiaKey, 1, []byte("jia: seq 1, fee 5"), 5, 100)
	res, err = n.Submit(txJia1)
	check(err)
	jia1 := res.TxID
	fmt.Printf("\n补交 甲 序号 1（费用 5，到期轮次 100）：%s\n", jia1)
	acct = n.Account(jiaPub)
	if acct.Gap != 0 || len(acct.Pending) != 2 ||
		acct.Pending[0].ID != jia1 || acct.Pending[1].ID != jia2 {
		log.Fatalf("补交后甲账户状态异常：%+v", acct)
	}
	fmt.Println("甲账户最早缺口变为 0，待处理按序号排列：")
	for _, p := range acct.Pending {
		printPending(p, senderName)
	}

	// 产生本地提议：每个账户只有“下一条可确认”的交易参与费用竞争，
	// 即乙 1（费用 20）与甲 1（费用 5）；甲 2 要排在甲 1 之后才有资格。
	// 乙 1 费用更高先入选，甲 1 次之，本块即满（上限 2），甲 2 继续排队。
	proposal, err := n.Propose()
	check(err)
	if len(proposal.TxIDs) != 2 || proposal.TxIDs[0] != yi1 || proposal.TxIDs[1] != jia1 {
		log.Fatalf("本地提议顺序应为 乙 1、甲 1，实际：%v", proposal.TxIDs)
	}
	fmt.Printf("\n本地提议（轮次 %d）：区块 %s\n", proposal.Round, proposal.BlockID)
	for i, id := range proposal.TxIDs {
		info, err := n.Tx(id)
		check(err)
		fmt.Printf("  打包顺序 %d：%s（账户=%s，序号=%d，费用=%d）\n",
			i+1, id, senderName[info.Tx.SenderHex()], info.Tx.Sequence, info.Tx.Fee)
	}

	// 提议后查询甲账户：甲 1 进入提议（waiting-vote），
	// 费用最高的甲 2 仍为 queued（waiting-pack）。
	acct = n.Account(jiaPub)
	if len(acct.Pending) != 2 || acct.Pending[1].ID != jia2 ||
		acct.Pending[1].Status != consensus.StatusQueued || acct.Pending[1].Note != "waiting-pack" {
		log.Fatalf("提议后甲账户状态异常：%+v", acct)
	}
	fmt.Println("提议后甲账户（费用最高的甲 2 未入选，仍等待打包）：")
	printAccount(acct, senderName)

	// 名单内三名不同验证者依次投票给本地提议：两票未达门槛，第三票确认。
	fmt.Println("\n—— 名单内三名验证者依次投票给本地提议 ——")
	for i := 0; i < 3; i++ {
		vr, err := n.Vote(validators[i], proposal.Round, proposal.BlockID)
		check(err)
		if !vr.Counted {
			log.Fatalf("验证者 %d 的投票应被计入", i)
		}
		if i < 2 && vr.Confirmed {
			log.Fatalf("验证者 %d 的投票不应确认（2 票未达 3 票门槛）", i)
		}
		if i == 2 && !vr.Confirmed {
			log.Fatal("第三票应使本地提议确认")
		}
		fmt.Printf("验证者 %d 投票：计入=%v，确认=%v", i, vr.Counted, vr.Confirmed)
		if vr.Confirmed {
			fmt.Printf("，新区块高度 %d", vr.Block.Height)
		}
		fmt.Println()
	}

	// 确认后：甲 1、乙 1 进入高度 1 的确认块；甲已确认序号推进为 1，
	// 待处理只剩甲 2（回到 queued），缺口仍为 0。
	infoJia1, err := n.Tx(jia1)
	check(err)
	infoYi1, err := n.Tx(yi1)
	check(err)
	if infoJia1.Status != consensus.StatusConfirmed || infoJia1.BlockHeight != 1 ||
		infoYi1.Status != consensus.StatusConfirmed || infoYi1.BlockHeight != 1 {
		log.Fatalf("甲 1 与乙 1 应在高度 1 确认：甲 1=%+v，乙 1=%+v", infoJia1, infoYi1)
	}
	fmt.Printf("甲 1：状态=%s，所在块高度=%d；乙 1：状态=%s，所在块高度=%d\n",
		infoJia1.Status, infoJia1.BlockHeight, infoYi1.Status, infoYi1.BlockHeight)
	acct = n.Account(jiaPub)
	if acct.ConfirmedSequence != 1 || len(acct.Pending) != 1 || acct.Gap != 0 ||
		acct.Pending[0].ID != jia2 || acct.Pending[0].Note != "waiting-pack" {
		log.Fatalf("确认后甲账户状态异常：%+v", acct)
	}
	fmt.Println("确认后甲账户：")
	printAccount(acct, senderName)

	// 下一轮本地提议只包含甲 2：说明变为 waiting-vote（已进入提议、等待投票），
	// 但甲已确认序号仍为 1——进入提议不等于得到确认。
	proposal2, err := n.Propose()
	check(err)
	if len(proposal2.TxIDs) != 1 || proposal2.TxIDs[0] != jia2 {
		log.Fatalf("下一轮本地提议应只包含甲 2，实际：%v", proposal2.TxIDs)
	}
	fmt.Printf("\n下一轮本地提议（轮次 %d）：区块 %s\n", proposal2.Round, proposal2.BlockID)
	for i, id := range proposal2.TxIDs {
		info, err := n.Tx(id)
		check(err)
		fmt.Printf("  打包顺序 %d：%s（账户=%s，序号=%d，费用=%d）\n",
			i+1, id, senderName[info.Tx.SenderHex()], info.Tx.Sequence, info.Tx.Fee)
	}
	acct = n.Account(jiaPub)
	if acct.ConfirmedSequence != 1 || len(acct.Pending) != 1 ||
		acct.Pending[0].ID != jia2 || acct.Pending[0].Status != consensus.StatusProposed ||
		acct.Pending[0].Note != "waiting-vote" {
		log.Fatalf("下一轮提议后甲账户状态异常：%+v", acct)
	}
	fmt.Println("下一轮提议后甲账户（甲 2 已进提议，但尚未确认）：")
	printAccount(acct, senderName)
}
