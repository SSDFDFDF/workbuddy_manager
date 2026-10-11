package upstream

import (
	"fmt"
	"strings"
	"testing"
)

// 标记还原层的热路径契约：还原层在**没有标记**的响应上只做逐帧扫描（O(每帧字节)），
// 不得引入每帧分配。弱判定常开（tools 声明缺失是泄漏常态），所以这条契约对所有
// 请求生效，不是「只影响启用了还原的请求」。基准守护「无标记时近似零开销」，
// 与 pipeline_bench_test.go（请求侧遍数）互补。
func dsmlBenchSSE(frames int) string {
	var b strings.Builder
	for i := 0; i < frames; i++ {
		fmt.Fprintf(&b, "data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"token%d \"}}]}\n\n", i)
	}
	b.WriteString("data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
	return b.String()
}

func BenchmarkConsumeNoMarkupRepair(b *testing.B) {
	raw := dsmlBenchSSE(200)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Aggregate(strings.NewReader(raw)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConsumeMarkupRepairEnabled(b *testing.B) {
	raw := dsmlBenchSSE(200)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// 与 handler 同形：每次出站尝试新建一个还原器（弱判定常开）。
		if _, err := Aggregate(strings.NewReader(raw), WithMarkupRepair(NewMarkupRepair(nil, true))); err != nil {
			b.Fatal(err)
		}
	}
}
