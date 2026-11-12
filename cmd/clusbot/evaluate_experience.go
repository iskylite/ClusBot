package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func runExperienceEvaluation(args []string) error {
	flags := flag.NewFlagSet("clusbot evaluate-experience", flag.ContinueOnError)
	db := flags.String("database", "", "只读 SQLite 数据库路径，建议使用备份")
	suitePath := flags.String("suite", "", "评测用例 JSON 路径")
	endpoint := flags.String("base-url", "", "模型 API 根地址")
	modelID := flags.String("model", "", "固定模型 ID")
	keyEnv := flags.String("key-env", "CLUSBOT_MODEL_API_KEY", "模型密钥的环境变量名")
	output := flags.Int("max-output-tokens", 1024, "每个用例每组的输出 Token 上限")
	timeout := flags.Duration("timeout", 30*time.Second, "单次模型请求的超时")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *db == "" || *suitePath == "" || *endpoint == "" || *modelID == "" {
		return errors.New("需要 --database、--suite、--base-url 和 --model")
	}
	suite, err := readExperienceEvaluationSuite(*suitePath)
	if err != nil {
		return err
	}
	provider, err := model.NewResponses(*endpoint, *modelID, os.Getenv(*keyEnv))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := work.ExperienceEvaluationConfig{
		Model: *modelID, ModelBinding: fmt.Sprintf("%x", sha256.Sum256([]byte(*endpoint+"\n"+*modelID))),
		MaxOutput: *output, RequestTimeout: *timeout,
	}
	report, err := work.EvaluateExperience(ctx, *db, suite, cfg, provider)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func readExperienceEvaluationSuite(path string) (work.ExperienceEvaluationSuite, error) {
	var suite work.ExperienceEvaluationSuite
	file, err := os.Open(path)
	if err != nil {
		return suite, err
	}
	defer file.Close()
	const maxSuiteBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(file, maxSuiteBytes+1))
	if err != nil {
		return suite, err
	}
	if len(body) > maxSuiteBytes || !json.Valid(body) {
		return suite, errors.New("评测用例需要有效的 JSON，大小不超过 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&suite)
	return suite, err
}
