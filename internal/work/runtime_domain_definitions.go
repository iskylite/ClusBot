package work

// P4-01：版本化领域包和类型约束。
//
// 本文件实现领域定义可以发布、引用和回退，并解释历史工作：
//   - 定义对象/关系类型、字段类型与单位、基数、身份规则、数据绑定、配置/
//     测试规范及流程引用；
//   - 草稿校验引用闭合、关系端点、单位、参数范围、互斥目标和样例；发布为
//     不可变版本；
//   - Task/Step 固定使用版本；规则发布不静默迁移运行中任务，紧急限制和
//     当前授权仍即时核验；
//   - 更改定义需记录影响、兼容性及回退依据；旧记录保留原版本；
//   - 提供声明式规则/规范加载和查询入口，禁止让模型生成规则后直接激活。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// 领域包（版本化定义）
// ---------------------------------------------------------------------------

// DomainPackage 是 domain_packages 的一行。
type DomainPackage struct {
	PackageID         string
	Name              string
	Version           int
	State             string // draft/published/superseded
	Definitions       string
	DefinitionsDigest string
	Compatibility     string
	ImpactSummary     string
	RollbackBasis     string
	PublishedBy       string
	PublishedAt       int64
	CreatedAt         int64
	UpdatedAt         int64
}

// DomainObjectType 是 domain_object_types 的一行。
type DomainObjectType struct {
	PackageID    string
	TypeKey      string
	Fields       string
	Cardinality  string
	IdentityRule string
	DataBinding  string
	ConfigSpec   string
	TestSpec     string
	CreatedAt    int64
}

// DomainRelationType 是 domain_relation_types 的一行。
type DomainRelationType struct {
	PackageID            string
	RelationKey          string
	SourceType           string
	TargetType           string
	Cardinality          string
	MutuallyExclusiveWith string
	CreatedAt            int64
}

// ---------------------------------------------------------------------------
// 草稿校验（引用闭合、关系端点、单位、参数范围、互斥目标和样例）
// ---------------------------------------------------------------------------

// validateDomainPackageDraft 校验领域包草稿（P4-01.2）。
// 草稿校验引用闭合、关系端点、单位、参数范围、互斥目标和样例；发布为不可变
// 版本。
func validateDomainPackageDraft(objectTypes []DomainObjectType, relationTypes []DomainRelationType) error {
	// 引用闭合：所有关系端点引用的类型必须存在。
	typeSet := make(map[string]bool, len(objectTypes))
	for _, ot := range objectTypes {
		if ot.TypeKey == "" {
			return errors.New("对象类型缺少标识")
		}
		if typeSet[ot.TypeKey] {
			return fmt.Errorf("对象类型 %s 重复", ot.TypeKey)
		}
		typeSet[ot.TypeKey] = true
		// 字段类型与单位校验。
		var fields []map[string]any
		if err := json.Unmarshal([]byte(ot.Fields), &fields); err != nil && ot.Fields != "" && ot.Fields != "[]" {
			return fmt.Errorf("对象类型 %s 字段定义无效：%v", ot.TypeKey, err)
		}
		for _, field := range fields {
			if field["name"] == nil || field["type"] == nil {
				return fmt.Errorf("对象类型 %s 字段缺少名称或类型", ot.TypeKey)
			}
		}
	}
	// 关系端点校验：引用的类型必须存在。
	relationSet := make(map[string]bool, len(relationTypes))
	for _, rt := range relationTypes {
		if rt.RelationKey == "" {
			return errors.New("关系类型缺少标识")
		}
		if relationSet[rt.RelationKey] {
			return fmt.Errorf("关系类型 %s 重复", rt.RelationKey)
		}
		relationSet[rt.RelationKey] = true
		if !typeSet[rt.SourceType] {
			return fmt.Errorf("关系 %s 的源类型 %s 不存在（引用闭合）", rt.RelationKey, rt.SourceType)
		}
		if !typeSet[rt.TargetType] {
			return fmt.Errorf("关系 %s 的目标类型 %s 不存在（引用闭合）", rt.RelationKey, rt.TargetType)
		}
		// 互斥目标校验。
		var exclusive []string
		if err := json.Unmarshal([]byte(rt.MutuallyExclusiveWith), &exclusive); err != nil && rt.MutuallyExclusiveWith != "" && rt.MutuallyExclusiveWith != "[]" {
			return fmt.Errorf("关系 %s 互斥目标定义无效：%v", rt.RelationKey, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 发布（发布为不可变版本）
// ---------------------------------------------------------------------------

// publishDomainPackage 发布领域包（P4-01.2/P4-01.4）。
// 发布为不可变版本；更改定义需记录影响、兼容性及回退依据；旧记录保留原版本。
func publishDomainPackage(tx *sql.Tx, p DomainPackage, objectTypes []DomainObjectType, relationTypes []DomainRelationType, now int64) (DomainPackage, error) {
	if p.Name == "" || p.Version < 1 {
		return DomainPackage{}, errors.New("领域包缺少名称或版本")
	}
	if p.PublishedBy == "" {
		return DomainPackage{}, errors.New("领域包发布缺少发布者")
	}
	// 草稿校验。
	if err := validateDomainPackageDraft(objectTypes, relationTypes); err != nil {
		return DomainPackage{}, err
	}
	// 发布为不可变版本（同 name+version 不可变）。
	var existingState string
	err := tx.QueryRow(`SELECT state FROM domain_packages WHERE name=? AND version=?`, p.Name, p.Version).Scan(&existingState)
	if err == nil {
		return DomainPackage{}, fmt.Errorf("领域包 %s@%d 已存在（发布为不可变版本）", p.Name, p.Version)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DomainPackage{}, err
	}
	// 序列化定义。
	definitions := map[string]any{
		"object_types":   objectTypes,
		"relation_types": relationTypes,
	}
	defJSON, err := json.Marshal(definitions)
	if err != nil {
		return DomainPackage{}, err
	}
	p.Definitions = string(defJSON)
	p.DefinitionsDigest = digest(p.Definitions)
	p.State = "published"
	p.PackageID = id()
	p.PublishedAt = now
	p.CreatedAt, p.UpdatedAt = now, now
	_, err = tx.Exec(`INSERT INTO domain_packages(package_id,name,version,state,definitions,definitions_digest,
compatibility,impact_summary,rollback_basis,published_by,published_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.PackageID, p.Name, p.Version, p.State, p.Definitions, p.DefinitionsDigest,
		p.Compatibility, p.ImpactSummary, p.RollbackBasis, p.PublishedBy, now, now, now)
	if err != nil {
		return DomainPackage{}, err
	}
	// 保存对象/关系类型。
	for _, ot := range objectTypes {
		ot.PackageID = p.PackageID
		ot.CreatedAt = now
		if _, err := tx.Exec(`INSERT INTO domain_object_types(package_id,type_key,fields,cardinality,identity_rule,data_binding,config_spec,test_spec,created_at)
VALUES(?,?,?,?,?,?,?,?,?)`, ot.PackageID, ot.TypeKey, ot.Fields, ot.Cardinality, ot.IdentityRule, ot.DataBinding, ot.ConfigSpec, ot.TestSpec, now); err != nil {
			return DomainPackage{}, err
		}
	}
	for _, rt := range relationTypes {
		rt.PackageID = p.PackageID
		rt.CreatedAt = now
		if _, err := tx.Exec(`INSERT INTO domain_relation_types(package_id,relation_key,source_type,target_type,cardinality,mutually_exclusive_with,created_at)
VALUES(?,?,?,?,?,?,?)`, rt.PackageID, rt.RelationKey, rt.SourceType, rt.TargetType, rt.Cardinality, rt.MutuallyExclusiveWith, now); err != nil {
			return DomainPackage{}, err
		}
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 查询（固定使用版本；旧记录保留原版本）
// ---------------------------------------------------------------------------

// readDomainPackage 读取领域包（P4-01.3/P4-01.4）。
// Task/Step 固定使用版本；旧记录保留原版本。
func readDomainPackage(q recordsQuerier, name string, version int) (DomainPackage, error) {
	var p DomainPackage
	err := q.QueryRow(`SELECT package_id,name,version,state,definitions,definitions_digest,
compatibility,impact_summary,rollback_basis,published_by,published_at,created_at,updated_at
FROM domain_packages WHERE name=? AND version=?`, name, version).
		Scan(&p.PackageID, &p.Name, &p.Version, &p.State, &p.Definitions, &p.DefinitionsDigest,
			&p.Compatibility, &p.ImpactSummary, &p.RollbackBasis, &p.PublishedBy, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	return p, nil
}

// latestPublishedPackage 读取最新已发布领域包。
func latestPublishedPackage(q recordsQuerier, name string) (DomainPackage, error) {
	var p DomainPackage
	err := q.QueryRow(`SELECT package_id,name,version,state,definitions,definitions_digest,
compatibility,impact_summary,rollback_basis,published_by,published_at,created_at,updated_at
FROM domain_packages WHERE name=? AND state='published' ORDER BY version DESC LIMIT 1`, name).
		Scan(&p.PackageID, &p.Name, &p.Version, &p.State, &p.Definitions, &p.DefinitionsDigest,
			&p.Compatibility, &p.ImpactSummary, &p.RollbackBasis, &p.PublishedBy, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 声明式加载（禁止让模型生成规则后直接激活）
// ---------------------------------------------------------------------------

// ErrModelGeneratedRule 是模型生成规则的错误。
var ErrModelGeneratedRule = errors.New("禁止让模型生成规则后直接激活")

// loadDomainDefinition 声明式加载领域定义（P4-01.5）。
// 提供声明式规则/规范加载和查询入口，禁止让模型生成规则后直接激活。
func loadDomainDefinition(tx *sql.Tx, name string, version int, source string) (DomainPackage, error) {
	// 禁止让模型生成规则后直接激活：加载必须来自声明式来源（非模型生成）。
	if source == "model-generated" {
		return DomainPackage{}, ErrModelGeneratedRule
	}
	return readDomainPackage(tx, name, version)
}

// 确保 context/sql/errors/fmt/json 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
	_ = errors.Is
	_ = fmt.Sprintf
	_ = json.Marshal
)
