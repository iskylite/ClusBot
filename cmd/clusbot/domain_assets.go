package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"clusbot/internal/work"
	"gopkg.in/yaml.v3"
)

func runImportDomainAssets(args []string) error {
	flags := flag.NewFlagSet("import-domain-assets", flag.ContinueOnError)
	directory := flags.String("data-dir", "./data", "ClusBot 数据目录")
	file := flags.String("file", "", "本地资产清单 YAML")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" || flags.NArg() != 0 {
		return errors.New("使用 import-domain-assets --file <资产清单> --data-dir <数据目录>")
	}
	source, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer source.Close()
	const manifestLimit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(source, manifestLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > manifestLimit {
		return errors.New("资产清单超过 1 MiB")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var manifest struct {
		Assets []work.DomainAssetEntry `yaml:"assets"`
	}
	if err := decoder.Decode(&manifest); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("资产清单须为一个 YAML 文档")
	}
	// A typed pointer decodes explicit YAML null the same as an omitted
	// field. Inspect only start nodes so blanks/null never enable a grant
	// immediately by falling back to legacy behavior.
	var windows struct {
		Assets []struct {
			Benchmark []struct {
				Start yaml.Node `yaml:"valid_from_unix_ms"`
			} `yaml:"benchmark_grants"`
		} `yaml:"assets"`
	}
	if err := yaml.Unmarshal(raw, &windows); err != nil {
		return err
	}
	for _, asset := range windows.Assets {
		for _, grant := range asset.Benchmark {
			if !grant.Start.IsZero() && grant.Start.ShortTag() != "!!int" {
				return errors.New("基准授权 valid_from_unix_ms 必须是 UNIX 毫秒整数，留空请删除该字段")
			}
		}
	}
	if err := os.MkdirAll(*directory, 0700); err != nil {
		return err
	}
	lock, err := work.LockDataDir(*directory)
	if err != nil {
		return err
	}
	defer lock.Close()
	store, err := work.Open(filepath.Join(*directory, "clusbot.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.ImportDomainAssets(context.Background(), manifest.Assets); err != nil {
		return err
	}
	fmt.Printf("已导入 %d 条资产配置\n", len(manifest.Assets))
	return nil
}
