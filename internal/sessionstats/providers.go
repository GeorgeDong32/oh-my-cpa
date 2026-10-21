package sessionstats

import (
	"path/filepath"
	"strings"
)

// providerForModel infers the vendor from a model id, mirroring the upstream
// provider_identity heuristics for the model families these parsers see. It
// returns "" when nothing matches — callers then keep their own default.
func providerForModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case model == "":
		return ""
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt") || strings.HasPrefix(model, "o1") ||
		strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") ||
		strings.HasPrefix(model, "chatgpt") || strings.HasPrefix(model, "codex-"):
		return "openai"
	case strings.HasPrefix(model, "gemini") || strings.HasPrefix(model, "gemma"):
		return "google"
	case strings.HasPrefix(model, "glm") || strings.HasPrefix(model, "chatglm"):
		return "zhipu"
	case strings.HasPrefix(model, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(model, "qwen") || strings.HasPrefix(model, "qwq"):
		return "alibaba"
	case strings.HasPrefix(model, "kimi") || strings.HasPrefix(model, "moonshot"):
		return "moonshot"
	case strings.HasPrefix(model, "grok"):
		return "xai"
	case strings.HasPrefix(model, "minimax") || strings.HasPrefix(model, "abab"):
		return "minimax"
	case strings.HasPrefix(model, "doubao") || strings.HasPrefix(model, "skylark"):
		return "bytedance"
	case strings.HasPrefix(model, "hunyuan"):
		return "tencent"
	case strings.HasPrefix(model, "ernie"):
		return "baidu"
	case strings.HasPrefix(model, "mistral") || strings.HasPrefix(model, "codestral") ||
		strings.HasPrefix(model, "ministral"):
		return "mistral"
	case strings.HasPrefix(model, "llama") || strings.HasPrefix(model, "meta-llama"):
		return "meta"
	case strings.HasPrefix(model, "mixtral"):
		return "mistral"
	case strings.HasPrefix(model, "command"):
		return "cohere"
	case strings.HasPrefix(model, "sonnet-flash") || strings.HasPrefix(model, "nova"):
		return "amazon"
	case strings.HasPrefix(model, "hunyuan-") || strings.HasPrefix(model, "yuanbao"):
		return "tencent"
	case strings.HasPrefix(model, "seed") || strings.HasPrefix(model, "doubao"):
		return "bytedance"
	}
	return ""
}

// canonicalProvider maps well-known provider aliases onto one spelling.
func canonicalProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "anthropic"
	case "openai", "openai.com":
		return "openai"
	case "google", "googleai", "google-ai", "gemini":
		return "google"
	case "zhipu", "zhipuai", "bigmodel":
		return "zhipu"
	case "alibaba", "qwen", "dashscope", "tongyi":
		return "alibaba"
	case "moonshot", "kimi":
		return "moonshot"
	case "xai", "x-ai", "grok":
		return "xai"
	case "minimax":
		return "minimax"
	case "bytedance", "volcengine", "volc":
		return "bytedance"
	case "tencent", "hunyuan":
		return "tencent"
	case "baidu":
		return "baidu"
	case "deepseek":
		return "deepseek"
	case "mistral":
		return "mistral"
	case "meta":
		return "meta"
	case "cohere":
		return "cohere"
	case "amazon", "bedrock":
		return "amazon"
	default:
		return strings.TrimSpace(provider)
	}
}

func fileDir(path string) string { return filepath.Dir(path) }

func dirBase(path string) string { return filepath.Base(path) }
