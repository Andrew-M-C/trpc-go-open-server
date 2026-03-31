package main

import (
	"errors"
	"flag"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Andrew-M-C/trpc-go-utils/log"
	"github.com/Andrew-M-C/trpc-go-utils/plugin"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	openaimodel "trpc.group/trpc-go/trpc-agent-go/model/openai"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/skill"
	trpc "trpc.group/trpc-go/trpc-go"
	thttp "trpc.group/trpc-go/trpc-go/http"
)

const serviceName = "trpc.open.skilledchatter.Chat"

// pluginsModelOpenAI 对应 trpc_go.yaml 中 plugins.model.openai 段。
type pluginsModelOpenAI struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

func main() {
	confFlag := flag.String("conf", "", "path to trpc_go.yaml (default: beside this main package)")
	flag.Parse()

	var cfgPath string
	var err error
	if strings.TrimSpace(*confFlag) != "" {
		cfgPath, err = filepath.Abs(strings.TrimSpace(*confFlag))
	} else {
		cfgPath, err = defaultTRPCConfigPath()
	}
	if err != nil {
		log.New().Err(err).Text("trpc config path").Fatal()
	}
	trpc.ServerConfigPath = cfgPath

	var openAICfg pluginsModelOpenAI
	plugin.Bind("model", "openai", &openAICfg)

	skillsRoot, err := skillsRootDir()
	if err != nil {
		log.New().Err(err).Text("skills root").Fatal()
	}
	repo, err := skill.NewFSRepository(skillsRoot)
	if err != nil {
		log.New().Err(err).Text("skill repository").Fatal()
	}

	s := trpc.NewServer()

	apiKey := strings.TrimSpace(openAICfg.APIKey)
	modelName := strings.TrimSpace(openAICfg.Model)
	baseURL := strings.TrimSpace(openAICfg.BaseURL)
	if apiKey == "" || modelName == "" {
		log.New().Text("plugins.model.openai: api_key and model are required in trpc_go.yaml").Fatal()
	}

	mdlOpts := []openaimodel.Option{openaimodel.WithAPIKey(apiKey)}
	if baseURL != "" {
		mdlOpts = append(mdlOpts, openaimodel.WithBaseURL(baseURL))
	}
	mdl := openaimodel.New(modelName, mdlOpts...)

	ag := llmagent.New(
		"skilled-chatter",
		llmagent.WithModel(mdl),
		llmagent.WithSkills(repo),
		llmagent.WithInstruction(
			"You are a helpful assistant. When the user needs factual "+
				"current time, use the current_time skill and skill_run as "+
				"documented in SKILL.md.",
		),
	)

	rnr := runner.NewRunner(
		"skilled-chatter",
		ag,
		runner.WithSessionService(inmemory.NewSessionService()),
	)
	defer func() {
		if err := rnr.Close(); err != nil {
			log.New().Err(err).Text("runner close").Warn()
		}
	}()

	thttp.RegisterNoProtocolServiceMux(s.Service(serviceName), newChatMux(rnr))

	log.New().Format("skilled-chatter listening (service=%s), config=%s, POST /chat",
		serviceName, cfgPath).Info()

	if err := s.Serve(); err != nil {
		log.New().Err(err).Text("serve").Fatal()
	}
}

func defaultTRPCConfigPath() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(file), "trpc_go.yaml")
	return filepath.Abs(p)
}

func skillsRootDir() (string, error) {
	return "./skills", nil // TODO: 待修改
}
