// Package qq 是 QQ 开放平台的接入层：出站调接口、入站收事件。
//
// 文件按「数据往哪个方向流」分四组，找代码先看这张表：
//
//	出站 HTTP   client.go   请求封装、错误码判定（新旧两码取或）、发送消息与被动回复
//	           token.go    access_token 获取与缓存（expires_in 宽容解码）
//	           send.go     发送请求/响应结构体（msg_type 0/2/7 是出站枚举）
//	出站 WS     gateway.go  长连接状态机：identify / resume 两段退避、心跳 ack 超时判死
//	入站        events.go   事件与消息结构体（message_type 0/3/101/102/103 是入站枚举，
//	                       与出站那套只有 0 重合，别混用）
//	           dedup.go    按 (事件类型, 消息 id) 判重 —— resume 会补发，必须常开
//	           hub.go      统一事件入口：WS 与 webhook 两个前端在这里汇成同一条后端
//	入站 HTTP   webhook.go  回调 handler（op 13 握手 / op 0 分发 / op 12 应答）
//	           signature.go ed25519 验签，仅 webhook 模式用得到
//
// 两个前端只在「谁产生 Event」这一步不同，解码、判重、派发全在 hub.go 一处，
// 所以后端功能不需要知道自己是跑在长连接上还是回调上。
//
// 测试分两种包名，都在本目录（Go 标准库 os / net/http 同形，go list 能查出来）：
//   - package qq_test  黑盒，只走导出 API。绝大多数测试该待在这里。
//   - package qq       白盒，本目录只留两个：gateway_test.go 要读退避阶梯与 session
//     状态机内部（canResume / nextDelay / identifyFailures / seq），client_test.go 要
//     改 c.tokens.now 造「只剩 30s 过期」并反序列化未导出的 tokenResponse。
//
// 黑盒不需要单独开目录：换包名就够了，而且 go test ./internal/qq 才能一次跑完并
// 报出真实覆盖率（挪出去就得加 -coverpkg，忘了加会拿到偏低的假数字）。
// 为了测试把实现细节导出，等于让测试反向决定包边界。
//
// 新增结构体字段前先看 testdata/ 里那两个真连样本，不要照文档想象报文形状。
package qq
