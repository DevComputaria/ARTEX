package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This document locks the ** capability limits** of the generic Webhook template.
//
// This is the only place in the package where "user-provided strings will be valued as code", so
// make sure you know what it can and cannot do, and test to fix these properties—otherwise someone
// will add a method to the template context, or add a readFile in the FuncMap, and the capabilities
// will be quietly expanded, and diff will look like just a harmless little function.

// TestTemplateContextHasNoMethods is the most important one.
//
// The template context is intended to be pure data (only the export fields, no methods).
//
// If this fails: It indicates that someone has added a method to webhookTemplateData / webhookItem.
// Before deciding to release the line, consider whether the method can be used by the template to
// read something you do not want exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s 暴露了 %d 个方法（%s）：text/template 可以调用它们，"+
				"等于把这些方法的能力开放给了模板作者", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal Locks the set of functions exposed to the template.
//
// Currently, only json / jsons are available, which serve to serialize values into JSON fragments —
// they cannot read files, requests, or execute commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("模板函数集合变了：得到 %v，期望 %v。新增函数前请确认它不会扩大能力面"+
			"（不能读写文件、不能发起网络请求、不能执行命令）", got, want)
	}
}

// TestTemplateCannotReachUnknownData Overrides cross-border access in the template: Access to
// something that does not exist must fail, not return anything; and the failure message must not
// bring out internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("访问不存在的字段应报错")
	}
	// The error cannot show the true content in the template context (vulnerability
	// title/abstract).
	for _, leak := range []string{"SQL注入", "参数 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("模板错误泄露了消息内容 %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently Template error is a configuration error and will not be fixed
// by retrying. If judged to be retryable, a bad template will cause three rounds of failure every
// time it is submitted.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("模板语法错误应在保存时就被拦下")
	}
	// Even if the proof is bypassed and the submission is made directly, it must be judged as a
	// permanent failure rather than repeated retries.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("坏模板应判永久失败，得到 %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON overrides the constraint that "the template's render results must
// be valid JSON." It also blocks uses like "generating plain text from a template to trigger other
// protocols."
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// Legal templates can pass.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("合法模板应通过校验: %v", err)
	}
	// Rendering non-JSON must be rejected (rather than sent as is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("渲染出非 JSON 应判永久失败，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "合法 JSON") {
		t.Errorf("错误信息应说明是 JSON 问题，得到 %v", err)
	}
}
