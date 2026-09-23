// Package models owns the model inventory: what exists, what is downloaded,
// and how much disk it uses.
//
// The core owns this and the Python worker owns *residency* — what is loaded
// right now. A missing model is a download problem the user fixes once; a model
// that will not fit in memory is a runtime problem tied to the machine's
// current state. Conflating them makes both harder to diagnose.
package models

// Kind separates recognition models from the bundled VAD.
type Kind string

const (
	KindASR Kind = "asr"
	KindVAD Kind = "vad"
)

// Entry is a model this build knows how to obtain.
type Entry struct {
	Kind     Kind
	Name     string
	Provider string

	// Repo is the Hugging Face repository. The CTranslate2 conversions rather
	// than the original OpenAI checkpoints: faster-whisper cannot load the
	// latter, and downloading two gigabytes to discover that is a poor
	// introduction.
	Repo string

	// ApproxBytes lets the UI say what a download will cost before it starts.
	// The difference between "large-v3" and "3 GB" matters on a metered
	// connection.
	ApproxBytes int64

	// Note is shown alongside the model in the UI.
	Note           string
	Recommendation string
	Accuracy       string
	Speed          string
	Hardware       string
	Language       string
	Tags           []string
}

// Catalog lists the models this build can download.
//
// A fixed list rather than "whatever the hub has": a user choosing between
// eight named options with descriptions is better served than one facing a
// search box, and the sizes are what make the choice informed.
var Catalog = []Entry{
	{
		Kind: KindASR, Name: "tiny", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-tiny", ApproxBytes: 78_000_000,
		Note:           "体积最小、速度最快，但漏字和错字明显。适合先确认整条流程能跑通，不适合作为正式成品。",
		Recommendation: "首次测试与排查环境", Accuracy: "较低", Speed: "最快",
		Hardware: "普通 CPU 与低内存设备", Language: "支持日语，但复杂对白表现有限", Tags: []string{"流程测试"},
	},
	{
		Kind: KindASR, Name: "base", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-base", ApproxBytes: 145_000_000,
		Note:           "比 tiny 稳定一些，仍能在笔记本 CPU 上轻快运行，适合快速草稿。",
		Recommendation: "快速草稿", Accuracy: "较低", Speed: "很快",
		Hardware: "普通 CPU，约 1 GB 可用内存", Language: "支持日语", Tags: []string{"低配置"},
	},
	{
		Kind: KindASR, Name: "small", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-small", ApproxBytes: 486_000_000,
		Note:           "速度、精度和占用比较均衡，是内存有限设备制作可用字幕的起点。",
		Recommendation: "低配置设备的正式任务", Accuracy: "中等", Speed: "较快",
		Hardware: "CPU 可用，建议 2 GB 以上可用内存", Language: "日语表现可用", Tags: []string{"低配置", "均衡"},
	},
	{
		Kind: KindASR, Name: "medium", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-medium", ApproxBytes: 1_530_000_000,
		Note:           "默认推荐。日语识别明显优于 small，CPU 仍可运行；大多数项目先选它。",
		Recommendation: "日语字幕的日常制作", Accuracy: "较高", Speed: "中等",
		Hardware: "CPU 可用，建议 4 GB 以上可用内存；GPU 更快", Language: "日语表现良好", Tags: []string{"日常推荐", "默认"},
	},
	{
		Kind: KindASR, Name: "large-v3", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-large-v3", ApproxBytes: 3_090_000_000,
		Note:           "准确度最高，适合最终成品和困难音频。CPU 上可能达到片长的 2–3 倍耗时，更适合 NVIDIA GPU。",
		Recommendation: "高精度成品与嘈杂音频", Accuracy: "最高", Speed: "较慢",
		Hardware: "建议 NVIDIA GPU；CPU 可运行但很慢，建议 8 GB 以上可用内存", Language: "多语言与日语表现最佳", Tags: []string{"高精度", "推荐 GPU"},
	},
	{
		Kind: KindASR, Name: "distil-large-v3", Provider: "faster-whisper",
		Repo: "Systran/faster-distil-whisper-large-v3", ApproxBytes: 1_510_000_000,
		Note:           "为英语蒸馏优化，英文接近 large-v3 且更快，但日语明显更弱。本项目处理日语时通常不要选择。",
		Recommendation: "英语素材", Accuracy: "英语较高，日语较低", Speed: "较快",
		Hardware: "CPU 或 GPU，建议 4 GB 以上可用内存", Language: "偏英语，不推荐日语", Tags: []string{"英语专用", "日语不推荐"},
	},
}

// Lookup finds a catalog entry.
func Lookup(kind Kind, name string) (Entry, bool) {
	for _, entry := range Catalog {
		if entry.Kind == kind && entry.Name == name {
			return entry, true
		}
	}
	return Entry{}, false
}

// ID renders a model's identifier, which is also its primary key.
func ID(kind Kind, name string) string {
	return string(kind) + ":" + name
}

// SplitID parses an identifier.
func SplitID(id string) (Kind, string, bool) {
	for i := range len(id) {
		if id[i] == ':' {
			kind := Kind(id[:i])
			if kind != KindASR && kind != KindVAD {
				return "", "", false
			}
			return kind, id[i+1:], true
		}
	}
	return "", "", false
}
