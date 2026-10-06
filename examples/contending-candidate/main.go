// 示例：四名验证者下，竞争候选通过投票胜出本地提议。
//
// 流程：建立节点并提交两笔交易 -> 本地提议打包两笔 -> 登记只含高费交易的
// 竞争候选 -> 一名验证者投本地提议、改投被拒 -> 三名验证者依次投竞争候选，
// 两票不够、第三票确认 -> 查询刚结束的轮次与两笔交易的最终状态。
//
// 运行：go run ./examples/contending-candidate
package main

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/bengzyyys/consensus-lab/consensus"
)

// check 处理正常调用的错误：任何非预期失败都直接终止演示。
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

func main() {
	log.SetFlags(0)

	// 四名验证者与两个互不相同的发送账户。
	validatorKeys := make([]ed25519.PrivateKey, 4)
	validators := make([][]byte, 4)
	for i := range validatorKeys {
		validatorKeys[i] = demoKey(byte(i + 1))
		validators[i] = append([]byte(nil), validatorKeys[i].Public().(ed25519.PublicKey)...)
	}
	aliceKey := demoKey(0xa1)
	bobKey := demoKey(0xb0)
	alicePub := aliceKey.Public().(ed25519.PublicKey)
	bobPub := bobKey.Public().(ed25519.PublicKey)

	// 在新的状态目录建立节点：固定种子、四名验证者、单块最多两笔交易。
	dir, err := os.MkdirTemp("", "consensus-demo-*")
	check(err)
	defer os.RemoveAll(dir)

	n, err := consensus.New(dir, consensus.Config{
		Seed:           []byte("contending-candidate-demo"),
		Validators:     validators,
		MaxTxsPerBlock: 2,
	})
	check(err)
	fmt.Printf("节点已建立：轮次 %d，单块上限 2，验证者 4 名（确认需严格超过 2/3，即至少 3 票）\n",
		n.CurrentRound())

	// 提交两笔交易：分属 Alice 与 Bob，序号均为 1，到期轮次 100 在演示期间不会到达，
	// 费用分别为 20 与 10。
	txAlice := consensus.NewTransaction(aliceKey, 1, []byte("alice: fee 20"), 20, 100)
	txBob := consensus.NewTransaction(bobKey, 1, []byte("bob: fee 10"), 10, 100)
	subAlice, err := n.Submit(txAlice)
	check(err)
	subBob, err := n.Submit(txBob)
	check(err)
	fmt.Printf("已提交 Alice 交易（费用 20）：%s\n", subAlice.TxID)
	fmt.Printf("已提交 Bob 交易（费用 10）：%s\n", subBob.TxID)

	// 本地提议：打包规则按费用从高到低选取各账户下一条可确认交易，
	// 上限两笔，故两笔都入选且费用 20 的排在前面。
	proposal, err := n.Propose()
	check(err)
	fmt.Printf("\n本地提议（轮次 %d）：区块 %s\n", proposal.Round, proposal.BlockID)
	for i, id := range proposal.TxIDs {
		fmt.Printf("  打包顺序 %d：%s\n", i+1, id)
	}

	// 登记竞争候选：只包含费用 20 的那笔交易，与本地提议同轮竞争。
	reg, err := n.RegisterCandidate(proposal.Round, []string{subAlice.TxID})
	check(err)
	fmt.Printf("竞争候选（仅含 Alice 交易）：区块 %s（新登记=%v）\n", reg.BlockID, !reg.Existing)

	// 保存轮次值：确认之后查询刚结束的轮次要用它。
	round := proposal.Round

	// 验证者 0 投给本地提议。
	vr, err := n.Vote(validators[0], round, proposal.BlockID)
	check(err)
	fmt.Printf("\n验证者 0 投给本地提议：计入=%v，确认=%v\n", vr.Counted, vr.Confirmed)

	// 验证者 1 投给竞争候选（第 1 票）。
	vr, err = n.Vote(validators[1], round, reg.BlockID)
	check(err)
	fmt.Printf("验证者 1 投给竞争候选：计入=%v，确认=%v\n", vr.Counted, vr.Confirmed)

	// 验证者 0 在确认前尝试改投竞争候选：预期被 already-voted 拒绝，原票保留。
	// 这是本演示中唯一预期出现的错误，须与其他失败区分开。
	_, err = n.Vote(validators[0], round, reg.BlockID)
	var rej *consensus.RejectError
	switch {
	case errors.As(err, &rej) && rej.Reason == consensus.ReasonAlreadyVoted:
		fmt.Printf("验证者 0 改投被拒绝（预期）：%v\n", rej)
	case err != nil:
		log.Fatalf("改投出现预期外的错误：%v", err)
	default:
		log.Fatal("改投被接受，违反每位验证者一轮一票的规则")
	}

	// 验证者 2 投给竞争候选（第 2 票）：四人名单要求严格超过 2/3，2 票仍不够。
	vr, err = n.Vote(validators[2], round, reg.BlockID)
	check(err)
	fmt.Printf("验证者 2 投给竞争候选：计入=%v，确认=%v（2 票未达 3 票门槛）\n", vr.Counted, vr.Confirmed)

	rc, err := n.Candidates(round)
	check(err)
	fmt.Printf("此时轮次 %d 仍未决出：\n", round)
	for _, c := range rc.Candidates {
		fmt.Printf("  候选 %s（本地=%v）结果=%s，票数=%d\n", short(c.BlockID), c.Local, c.Result, len(c.Voters))
	}

	// 验证者 3 投出第 3 票：严格超过 4 的 2/3，竞争候选立即确认。
	vr, err = n.Vote(validators[3], round, reg.BlockID)
	check(err)
	if !vr.Confirmed || vr.Block == nil {
		log.Fatal("第 3 票应使竞争候选立即确认")
	}
	fmt.Printf("验证者 3 投给竞争候选：计入=%v，确认=%v，新区块高度 %d\n", vr.Counted, vr.Confirmed, vr.Block.Height)

	// 用保存的轮次值查询刚结束的轮次：竞争候选胜出、本地提议落选，各自投票者保留。
	rc, err = n.Candidates(round)
	check(err)
	fmt.Printf("\n轮次 %d 已决出（节点当前轮次 %d）：\n", round, n.CurrentRound())
	for _, c := range rc.Candidates {
		fmt.Printf("  候选 %s（本地=%v）结果=%s，投票者：\n", short(c.BlockID), c.Local, c.Result)
		for _, v := range c.Voters {
			fmt.Printf("    %x\n", v)
		}
	}

	// 费用 20 的交易随胜出块确认；其账户已确认序号推进为 1。
	infoAlice, err := n.Tx(subAlice.TxID)
	check(err)
	fmt.Printf("\nAlice 交易：状态=%s，所在块高度=%d，区块=%s\n",
		infoAlice.Status, infoAlice.BlockHeight, infoAlice.BlockID)
	accAlice := n.Account(alicePub)
	fmt.Printf("Alice 账户：已确认序号=%d，待处理交易=%d 笔\n",
		accAlice.ConfirmedSequence, len(accAlice.Pending))

	// 仅被本地提议引用的 Bob 交易回到排队；其账户已确认序号仍为 0，
	// 账户查询显示它在等待打包。
	infoBob, err := n.Tx(subBob.TxID)
	check(err)
	fmt.Printf("Bob 交易：状态=%s\n", infoBob.Status)
	accBob := n.Account(bobPub)
	fmt.Printf("Bob 账户：已确认序号=%d\n", accBob.ConfirmedSequence)
	for _, p := range accBob.Pending {
		fmt.Printf("  待处理 %s：状态=%s，说明=%s\n", short(p.ID), p.Status, p.Note)
	}

	// 落选的本地提议不会生成另一条确认历史：确认历史只有胜出块一条。
	latest, ok := n.LatestBlock()
	if !ok {
		log.Fatal("应已有确认块")
	}
	fmt.Printf("\n确认历史共 %d 条，最新块高度 %d、区块 %s（即胜出的竞争候选）\n",
		n.Height(), latest.Height, latest.ID)
}
