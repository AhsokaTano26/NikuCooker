// Package settings holds the configuration the web interface can change.
//
// Configuration a user may edit lives in two places, and the split is by who
// is expected to do the editing. Anything a person would reasonably want to
// tune — how accurate the recognition is, how fast the subtitles read, whether
// the render burns them in — is described here and editable in the browser.
// Anything that is a property of the machine or of the install — where the
// data lives, which FFmpeg to run, how many Python workers to start — is not,
// and stays in the configuration file where it can be set once and left.
//
// The metadata lives here rather than in the frontend so that a setting's
// name and meaning have one author. Two tables — one in Go and one in
// TypeScript — drift, and the drift is invisible until someone reads a label
// that describes an older version of the field.
package settings

// Kind is how a value is entered and checked.
type Kind string

const (
	KindBool   Kind = "bool"
	KindInt    Kind = "int"
	KindFloat  Kind = "float"
	KindString Kind = "string"
	KindEnum   Kind = "enum"
	KindList   Kind = "list"
	KindBytes  Kind = "bytes"
)

// Option is one choice of an enum, or one member of a fixed list.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Setting describes one editable key.
type Setting struct {
	// Key is the configuration document path, e.g. "asr.model". It is the same
	// dotted path the provenance uses, so the two can be joined without a
	// translation table between them.
	Key string

	// Name and Help are what the interface shows. Chinese, because that is
	// what this tool's users read, and written as one line of what it does
	// rather than restating the key in words.
	Name string
	Help string

	Group string

	Kind    Kind
	Options []Option

	// Unit is appended to the value where one is meaningful, e.g. "字/秒".
	Unit string

	// Min, Max and Step bound a number. Zero means unbounded, which is the
	// common case.
	Min  float64
	Max  float64
	Step float64

	// Advanced marks a setting most people never touch. It is still editable;
	// it is folded away so that the twenty that matter are readable.
	Advanced bool
}

// Groups, in the order the interface shows them.
//
// Ordered here rather than by the order settings happen to be declared,
// because the sequence a person reads them in is a decision.
var Groups = []string{
	"识别",
	"字幕",
	"翻译",
	"语音检测",
	"质量检查",
	"渲染",
	"音频提取",
	"服务端",
	"流水线",
}

// Catalog is every setting the interface can change.
//
// Every key here is applied on the next run without a restart, which is not a
// coincidence: the ones that could not be are absent. See NotEditable for what
// was left out and why.
var Catalog = []Setting{
	// -----------------------------------------------------------------------
	// 识别
	// -----------------------------------------------------------------------
	{
		Key: "asr.model", Name: "识别模型", Group: "识别", Kind: KindEnum,
		Help: "语音转文字的模型。越大越准，也越慢、越占内存。",
		Options: []Option{
			{Value: "tiny", Label: "tiny — 74 MB，最快，用来试通流程"},
			{Value: "base", Label: "base — 138 MB"},
			{Value: "small", Label: "small — 464 MB，小内存机器的下限"},
			{Value: "medium", Label: "medium — 1.4 GB，默认"},
			{Value: "large-v3", Label: "large-v3 — 2.9 GB，最准"},
			{Value: "distil-large-v3", Label: "distil-large-v3 — 1.4 GB，接近 large-v3 但偏英语"},
		},
	},
	{
		Key: "asr.device", Name: "运行设备", Group: "识别", Kind: KindEnum,
		Help: "auto 会自动挑。CUDA 需要 NVIDIA 显卡，没有时会退回 CPU 并记录一条警告。",
		Options: []Option{
			{Value: "auto", Label: "自动"},
			{Value: "cpu", Label: "CPU"},
			{Value: "cuda", Label: "NVIDIA GPU (CUDA)"},
		},
	},
	{
		Key: "asr.language", Name: "原语言", Group: "识别", Kind: KindString,
		Help: "留空则自动检测。已知时填上更快也更准，例如 ja。",
	},
	{
		Key: "asr.beam_size", Name: "搜索宽度", Group: "识别", Kind: KindInt,
		Help: "越大越准也越慢。5 是默认。", Min: 1, Max: 10, Advanced: true,
	},

	// -----------------------------------------------------------------------
	// 字幕
	// -----------------------------------------------------------------------
	{
		Key: "subtitle.formats", Name: "输出格式", Group: "字幕", Kind: KindList,
		Help: "两个都写。SRT 到处都能播；ASS 带样式，也是渲染阶段烧录用的那份。",
		Options: []Option{
			{Value: "srt", Label: "SRT"},
			{Value: "ass", Label: "ASS"},
		},
	},
	{
		Key: "subtitle.preset", Name: "字幕样式", Group: "字幕", Kind: KindEnum,
		Help: "ASS 的字体、描边和大小。烧录进画面时用的就是这一套。",
		Options: []Option{
			{Value: "fansub", Label: "字幕组 — 描边加阴影，适合动画"},
			{Value: "default", Label: "默认 — 朴素"},
			{Value: "broadcast", Label: "电视 — 字号更大，行更短"},
		},
	},
	{
		Key: "subtitle.bilingual", Name: "双语字幕", Group: "字幕", Kind: KindBool,
		Help: "原文在上、译文在下。",
	},
	{
		Key: "subtitle.max_cps", Name: "阅读速度上限", Group: "字幕", Kind: KindFloat,
		Help: "每秒多少个字。超出的行会被标进审校队列，但不会被自动切断。",
		Unit: "字/秒", Min: 4, Max: 40, Step: 1,
	},
	{
		Key: "subtitle.max_chars_zh", Name: "中文字数上限", Group: "字幕", Kind: KindInt,
		Help: "单行最多几个字。", Min: 8, Max: 80,
	},
	{
		Key: "subtitle.max_chars_ja", Name: "日文字数上限", Group: "字幕", Kind: KindInt,
		Help: "拆分原文时的长度上限。", Min: 8, Max: 80, Advanced: true,
	},
	{
		Key: "subtitle.min_duration", Name: "最短停留", Group: "字幕", Kind: KindFloat,
		Help: "一行至少显示多久。太短的会往前借时间。", Unit: "秒", Min: 0.2, Max: 5, Step: 0.1, Advanced: true,
	},
	{
		Key: "subtitle.max_duration", Name: "最长停留", Group: "字幕", Kind: KindFloat,
		Help: "超过就断开。", Unit: "秒", Min: 2, Max: 30, Step: 0.5, Advanced: true,
	},
	{
		Key: "subtitle.min_gap", Name: "行间最小间隔", Group: "字幕", Kind: KindFloat,
		Help: "相邻两行之间留的空白。为零会让两行看起来像一行。",
		Unit: "秒", Min: 0, Max: 1, Step: 0.02, Advanced: true,
	},
	{
		Key: "subtitle.pause_ms", Name: "停顿断句阈值", Group: "字幕", Kind: KindInt,
		Help: "词与词之间静默超过这么久就在这里断句。", Unit: "毫秒", Min: 100, Max: 2000, Step: 50, Advanced: true,
	},

	// -----------------------------------------------------------------------
	// 翻译
	// -----------------------------------------------------------------------
	{
		Key: "translation.style", Name: "翻译风格", Group: "翻译", Kind: KindEnum,
		Help: "新建项目时的默认风格，项目里可以单独改。",
		Options: []Option{
			{Value: "fansub", Label: "字幕组 — 保留敬称与圈内惯用语，简洁优先"},
			{Value: "natural", Label: "意译 — 按中文语序重写"},
			{Value: "literal", Label: "直译 — 贴近日语结构与敬语"},
		},
	},
	{
		Key: "translation.batch_size", Name: "每批行数", Group: "翻译", Kind: KindInt,
		Help: "一次请求翻译多少行。批量越大请求越少，但一批失败时重来的代价也越大。",
		Min:  1, Max: 100,
	},
	{
		Key: "translation.context_lines", Name: "上下文行数", Group: "翻译", Kind: KindInt,
		Help: "每批附带前后各几行作为参考。只读，不翻译。", Min: 0, Max: 10,
	},
	{
		Key: "translation.concurrency", Name: "并发请求数", Group: "翻译", Kind: KindInt,
		Help: "同时发几个请求。调太高容易触发服务商的限流。", Min: 1, Max: 16,
	},
	{
		Key: "translation.temperature", Name: "随机度", Group: "翻译", Kind: KindFloat,
		Help: "越低越稳定、越可复现。", Min: 0, Max: 2, Step: 0.1, Advanced: true,
	},
	{
		Key: "translation.max_retries", Name: "失败重试次数", Group: "翻译", Kind: KindInt,
		Help: "网络抖动或限流时重试几次。", Min: 0, Max: 10, Advanced: true,
	},
	{
		Key: "translation.max_tokens_per_job", Name: "单次运行 token 上限", Group: "翻译", Kind: KindInt,
		Help: "到上限就报错停下，而不是静默截断。0 表示不限。", Min: 0, Advanced: true,
	},
	{
		Key: "translation.cache_max_entries", Name: "译文缓存条数", Group: "翻译", Kind: KindInt,
		Help: "逐行译文缓存的上限，按最近使用时间清理。", Min: 0, Advanced: true,
	},

	// -----------------------------------------------------------------------
	// 语音检测
	// -----------------------------------------------------------------------
	{
		Key: "vad.enabled", Name: "启用语音检测", Group: "语音检测", Kind: KindBool,
		Help: "先找出有人说话的区间，只把这些片段送去识别。关掉会明显变慢。",
	},
	{
		Key: "vad.threshold", Name: "语音判定阈值", Group: "语音检测", Kind: KindFloat,
		Help: "越低越容易把音乐和噪声当成说话。", Min: 0.1, Max: 0.9, Step: 0.05,
	},
	{
		Key: "vad.min_speech_ms", Name: "最短语音段", Group: "语音检测", Kind: KindInt,
		Help: "短于这个时长的片段会被丢掉，用来滤掉咳嗽和杂音。", Unit: "毫秒", Min: 50, Max: 2000, Step: 50, Advanced: true,
	},
	{
		Key: "vad.min_silence_ms", Name: "最短静音段", Group: "语音检测", Kind: KindInt,
		Help: "静音超过这么久才算一段的结束。", Unit: "毫秒", Min: 100, Max: 3000, Step: 50, Advanced: true,
	},
	{
		Key: "vad.speech_pad_ms", Name: "片段前后留白", Group: "语音检测", Kind: KindInt,
		Help: "每段前后各多留一点，避免把字头字尾切掉。", Unit: "毫秒", Min: 0, Max: 1000, Step: 10, Advanced: true,
	},
	{
		Key: "vad.max_speech_s", Name: "最长语音段", Group: "语音检测", Kind: KindFloat,
		Help: "超过就切开，避免一次送进去太长。", Unit: "秒", Min: 5, Max: 120, Step: 5, Advanced: true,
	},

	// -----------------------------------------------------------------------
	// 质量检查
	// -----------------------------------------------------------------------
	{
		Key: "qc.enabled", Name: "启用质量检查", Group: "质量检查", Kind: KindBool,
		Help: "检查阅读速度、时长、重复、术语等，结果进审校队列。",
	},
	{
		Key: "qc.llm", Name: "让模型复核", Group: "质量检查", Kind: KindBool,
		Help: "再调用一次模型检查译文质量。更准，也更贵、更慢。",
	},
	{
		Key: "qc.max_cps", Name: "阅读速度上限", Group: "质量检查", Kind: KindFloat,
		Help: "超过这个速度就报一条问题。", Unit: "字/秒", Min: 4, Max: 40, Step: 1,
	},
	{
		Key: "qc.duplicate_window", Name: "重复检测窗口", Group: "质量检查", Kind: KindInt,
		Help: "往前看几行来判定重复。识别模型打转时会出现成片重复。", Unit: "行", Min: 1, Max: 20, Advanced: true,
	},

	// -----------------------------------------------------------------------
	// 渲染
	// -----------------------------------------------------------------------
	{
		Key: "render.modes", Name: "渲染版本", Group: "渲染", Kind: KindList,
		Help: "两个可以同时要，一次运行产出两份。hard 是整片重编码，慢，而且需要带 libass 的 FFmpeg。",
		Options: []Option{
			{Value: "soft", Label: "软字幕 — 字幕作为独立轨道封装，视频流直接复制，几秒钟"},
			{Value: "hard", Label: "硬字幕 — 把字幕画进画面，整片重编码"},
		},
	},
	{
		Key: "render.crf", Name: "画质", Group: "渲染", Kind: KindInt,
		Help: "只影响硬字幕。数字越小画质越好、文件越大。18 约等于视觉无损。",
		Min:  0, Max: 51, Advanced: true,
	},
	{
		Key: "render.preset", Name: "编码速度", Group: "渲染", Kind: KindEnum,
		Help: "只影响硬字幕。越慢压得越好。",
		Options: []Option{
			{Value: "ultrafast", Label: "最快"},
			{Value: "veryfast", Label: "很快"},
			{Value: "medium", Label: "中等（默认）"},
			{Value: "slow", Label: "慢 — 更小"},
			{Value: "veryslow", Label: "最慢 — 最小"},
		},
		Advanced: true,
	},
	{
		Key: "render.audio_bitrate", Name: "音频码率", Group: "渲染", Kind: KindString,
		Help: "只影响硬字幕（软字幕的音频是直接复制的）。例如 192k。", Advanced: true,
	},

	// -----------------------------------------------------------------------
	// 音频提取
	// -----------------------------------------------------------------------
	{
		Key: "audio.highpass_hz", Name: "低频滤除", Group: "音频提取", Kind: KindInt,
		Help: "滤掉这个频率以下的隆隆声，只留人声所在的频段。", Unit: "Hz", Min: 0, Max: 300, Step: 10, Advanced: true,
	},
	{
		Key: "audio.loudnorm", Name: "响度归一化", Group: "音频提取", Kind: KindBool,
		Help: "把整体音量拉齐。默认关闭，因为它改变了模型看到的信号。", Advanced: true,
	},

	// -----------------------------------------------------------------------
	// 服务端
	// -----------------------------------------------------------------------
	{
		Key: "server.allow_path_source", Name: "允许服务端路径建项目", Group: "服务端", Kind: KindBool,
		Help: "打开后，新建项目时可以直接填服务器上的文件路径。它会读取进程能访问的任意文件，" +
			"不只是视频，所以只有在能访问这个端口的人都可信时才打开。",
	},
	{
		Key: "server.max_upload_bytes", Name: "上传大小上限", Group: "服务端", Kind: KindBytes,
		Help: "单个文件的上传上限。超过会在传输开始前被拒绝，不会白传一遍。",
	},

	// -----------------------------------------------------------------------
	// 流水线
	// -----------------------------------------------------------------------
	{
		Key: "pipeline.disabled", Name: "跳过的阶段", Group: "流水线", Kind: KindList,
		Help: "选中的可选阶段不会运行。润色会把每一行再让模型过一遍，成本大约翻倍。",
		Options: []Option{
			{Value: "polish", Label: "润色 — 二次润色，成本翻倍"},
			{Value: "context", Label: "背景分析 — 读一遍全篇生成作品背景"},
			{Value: "render", Label: "渲染 — 产出带字幕的视频"},
		},
	},
}

// NotEditable lists the configuration left out of Catalog, and why.
//
// Kept here rather than in a comment somewhere else because "why can't I
// change this in the browser" is a question someone will ask while looking at
// this file, and the answer is a property of the setting.
//
// The three groups are: things captured before the server starts listening,
// things memoized behind a sync.Once, and the language-model endpoint.
var NotEditable = []struct {
	Prefix string
	Why    string
}{
	{"storage.", "数据目录在启动时就打开了，改了要重启才能生效"},
	{"server.host", "监听地址由启动时的命令行参数决定"},
	{"server.port", "监听端口由启动时的命令行参数决定"},
	{"ai.", "Python 解释器在第一次用到时就固定下来了"},
	{"media.", "FFmpeg 路径在启动时交给了媒体服务"},
	{"worker.", "工作进程池在第一次用到时按当时的大小建好"},
	{"artifact.", "产物缓存的保留策略在启动时读取"},
	{"translation.base_url", "翻译服务在「翻译服务」页配置，那里密钥不会回传给浏览器"},
	{"translation.api_key", "翻译服务在「翻译服务」页配置，那里密钥不会回传给浏览器"},
	{"translation.model", "翻译服务在「翻译服务」页配置，那里密钥不会回传给浏览器"},
}

// ByKey indexes the catalog.
var ByKey = func() map[string]Setting {
	index := make(map[string]Setting, len(Catalog))
	for _, setting := range Catalog {
		index[setting.Key] = setting
	}
	return index
}()

// Lookup returns the setting for a document path.
func Lookup(key string) (Setting, bool) {
	setting, ok := ByKey[key]
	return setting, ok
}

// Grouped returns the catalog arranged for display, in Groups order.
func Grouped() []struct {
	Group    string
	Settings []Setting
} {
	out := make([]struct {
		Group    string
		Settings []Setting
	}, 0, len(Groups))

	for _, group := range Groups {
		grouped := make([]Setting, 0, 8)
		for _, setting := range Catalog {
			if setting.Group == group {
				grouped = append(grouped, setting)
			}
		}
		if len(grouped) > 0 {
			out = append(out, struct {
				Group    string
				Settings []Setting
			}{Group: group, Settings: grouped})
		}
	}
	return out
}
