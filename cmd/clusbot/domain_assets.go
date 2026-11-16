package main

import (
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
	decoder := yaml.NewDecoder(io.LimitReader(source, 1<<20))
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
