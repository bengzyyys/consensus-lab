// Package consensus 是本地共识与交易池仿真。
//
// 典型流程：
//
//	n, err := consensus.New(dir, consensus.Config{
//	    Seed: seed, Validators: validatorKeys, MaxTxsPerBlock: 10,
//	})
//	tx := consensus.NewTransaction(priv, 1, []byte("hello"), 5, 100)
//	if _, err := n.Submit(tx); err != nil { … }
//	p, err := n.Propose()                 // 每轮唯一提议，重复调用返回同一结果
//	n.Vote(validator, p.Round, p.BlockID) // 票数严格超过 2/3 立即确认
//
// 停止后用 Open(dir) 恢复轮次、交易池、未决提议、票数与确认历史。
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
