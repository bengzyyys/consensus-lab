package consensus

import "strings"

// 交易池在三处按同一条优先级规则取舍交易：打包时在各账户下一条可确认交易中
// 选最优、池满时在可淘汰的排队交易中找最弱、判断新交易是否值得挤出旧交易。
// 该高低关系只在 cmpTxPriority 一处表达，三处共用，保证取舍口径完全一致：
//
//	费用更高者优先；费用相同时，交易标识字典序更小者优先。
//
// 费用按 uint64 原生比较，费用为 0 或最大值、两笔费用相差悬殊时都能正确
// 区分高低，不会溢出、反转或被当成相同费用；同费时以完整交易标识定序。
//
// cmpTxPriority 返回正数表示 (fee, id) 严格优先于 (otherFee, otherID)，
// 负数表示严格更弱，0 表示同费同标识。同费时标识更小者优先，故对
// strings.Compare 的结果取反。同一标识的交易在提交入口已按重复拒绝，
// 取舍路径中不会出现 0。
func cmpTxPriority(fee uint64, id string, otherFee uint64, otherID string) int {
	switch {
	case fee > otherFee:
		return 1
	case fee < otherFee:
		return -1
	default:
		return -strings.Compare(id, otherID)
	}
}
