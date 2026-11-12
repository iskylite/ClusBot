package mcpclient

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

var ErrToolSchema = errors.New("工具定义不兼容")

// ValidateToolSchemas also checks definitions persisted by earlier releases.
// Never fetch a reference from a server-supplied URL during discovery or a call.
func ValidateToolSchemas(tool Tool) error {
	if _, err := compileSchema(tool.InputSchema); err != nil {
		return fmt.Errorf("%w：输入 %s", ErrToolSchema, err)
	}
	if len(tool.OutputSchema) != 0 {
		if _, err := compileSchema(tool.OutputSchema); err != nil {
			return fmt.Errorf("%w：输出 %s", ErrToolSchema, err)
		}
	}
	return nil
}

func compileSchema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil || node == nil || node["type"] != "object" {
		return nil, errors.New("Schema 必须声明对象类型")
	}
	if err := checkDialects(node, "2020-12", 0); err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, errors.New("Schema 结构无效")
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, errors.New("Schema 无法编译，请检查约束、正则表达式和本地引用；不支持外部引用")
	}
	return resolved, nil
}

func checkDialects(value any, inherited string, depth int) error {
	if depth > 64 {
		return errors.New("Schema 嵌套超过 64 层")
	}
	node, ok := value.(map[string]any)
	if !ok { // Boolean subschemas are supported by the validator.
		return nil
	}
	dialect := inherited
	if declared, present := node["$schema"]; present {
		switch declared {
		case "https://json-schema.org/draft/2020-12/schema":
			dialect = "2020-12"
		case "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema#":
			dialect = "draft-07"
		default:
			return errors.New("Schema 方言不受支持；支持 2020-12 与 draft-07")
		}
		if depth > 0 && dialect != inherited {
			return errors.New("Schema 混合了不同方言")
		}
	}
	if vocabularies, ok := node["$vocabulary"].(map[string]any); ok {
		for uri, required := range vocabularies {
			if required != true {
				continue
			}
			switch uri {
			case "https://json-schema.org/draft/2020-12/vocab/core",
				"https://json-schema.org/draft/2020-12/vocab/applicator",
				"https://json-schema.org/draft/2020-12/vocab/unevaluated",
				"https://json-schema.org/draft/2020-12/vocab/validation",
				"https://json-schema.org/draft/2020-12/vocab/meta-data",
				"https://json-schema.org/draft/2020-12/vocab/format-annotation",
				"https://json-schema.org/draft/2020-12/vocab/content":
			default:
				return errors.New("Schema 包含不支持的必需词汇表")
			}
		}
	}
	// Only traverse schema-bearing keywords, not user examples/default values.
	for _, key := range []string{"$defs", "definitions", "properties", "patternProperties", "dependentSchemas", "dependencies"} {
		if children, ok := node[key].(map[string]any); ok {
			for _, child := range children {
				if err := checkDialects(child, dialect, depth+1); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"} {
		if children, ok := node[key].([]any); ok {
			for _, child := range children {
				if err := checkDialects(child, dialect, depth+1); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"items", "additionalItems", "contains", "unevaluatedItems", "additionalProperties", "propertyNames", "unevaluatedProperties", "not", "if", "then", "else", "contentSchema"} {
		if err := checkDialects(node[key], dialect, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func validateOutput(tool Tool, structured any) string {
	if len(tool.OutputSchema) == 0 {
		return ""
	}
	schema, err := compileSchema(tool.OutputSchema)
	if err != nil {
		return "输出定义无法校验，请重新测试连接"
	}
	if structured == nil {
		return "工具未返回输出定义要求的结构化数据"
	}
	if err = schema.Validate(structured); err != nil {
		// Validator diagnostics may contain source values or credentials.
		return "结构化结果不符合工具的输出定义，请核对原始结果"
	}
	return ""
}
