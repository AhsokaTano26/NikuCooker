package api

import "testing"

// Replacing any label with an English display name must fail this test. The
// API owns localisation, so every client receives the same Chinese stage list.
func TestStageLabelsAreChinese(t *testing.T) {
	tests := map[string]string{
		"probe":        "读取媒体信息",
		"audio":        "提取音频",
		"vad":          "检测语音",
		"asr":          "语音识别",
		"segmentation": "切分字幕",
		"context":      "分析上下文",
		"translation":  "翻译字幕",
		"polish":       "润色译文",
		"qc":           "质量检查",
		"subtitle":     "生成字幕文件",
		"render":       "渲染视频",
	}

	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			if got := stageLabel(name); got != want {
				t.Errorf("stageLabel(%q) = %q, want %q", name, got, want)
			}
		})
	}
}
