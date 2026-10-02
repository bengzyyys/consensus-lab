// Package consensus 是本地共识与交易池仿真。
//
// 典型流程：
//
//	n, err := consensus.New(dir, consensus.Config{
//	    Seed: seed, Validators: validatorKeys, MaxTxsPerBlock: 10,
//	    PoolCapacity: 100, // 0 或省略表示交易池不限制
//	})
//	tx := consensus.NewTransaction(priv, 1, []byte("hello"), 5, 100)
//	if _, err := n.Submit(tx); err != nil { … }
//	p, err := n.Propose()                  // 每轮唯一本地提议，重复调用返回同一结果
//	// 本地提议产生后、确认前，可按交易顺序登记其他竞争候选：
//	r, err := n.RegisterCandidate(p.Round, []string{tx.ID()})
//	n.Vote(validator, p.Round, p.BlockID)  // 可投给任一已登记候选，票数严格超过 2/3 立即确认
//	rc, _ := n.Candidates(p.Round)         // 查看本轮候选、投票者、结果与未投票验证者
//
// 同一轮可有多个候选块竞争：候选使用当前轮次与下一高度，接在最新确认块之后；
// 每位验证者一轮只能选择一个候选，胜出候选立即确认，其他候选落选，其独有交易回池。
//
// 停止后用 Open(dir) 恢复轮次、交易池、未决候选、每人已投的选择、票数与确认历史。
// 版本 1 的旧状态目录仍可打开：未决提议与票数作为本地候选恢复，旧轮候选详情无记录。
//
// 稳定编码规则（跨语言可复现）：
//   - 签名编码 SigningBytes：前缀 "consensus-lab-tx-v1\n" 后接 canonical JSON
//     （UTF-8、无空白、字段按名字典序）：
//     {"content":<base64>,"expiry":<uint64>,"fee":<uint64>,"sender":<base64>,"sequence":<uint64>}
//     签名为标准 Ed25519。
//   - 交易标识 Transaction.ID：在上述 JSON 上追加 "signature":<base64> 字段（字段按名字典序），
//     对整体取 SHA-256 的十六进制摘要。
//   - 区块标识 BlockID：对 {"round","height","previous_id","transactions"} 的 canonical JSON
//     取 SHA-256 十六进制摘要；transactions 为按提议顺序排列的交易标识（空块为空数组）。
//   - 每轮提议者：SHA-256(seed || big-endian uint64 round) 的前 8 字节大端整数
//     对验证者人数取模。
package consensus

// Ready 表示基线可以运行。
func Ready() bool { return true }
