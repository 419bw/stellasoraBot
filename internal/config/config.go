// Package config 是部署配置文件的通用机制：把一个 YAML 读进结构体，并把"配错了会
// 静默改变行为"的那几种写法挡在启动之前——键名拼错、结构体声明的键没给值、值写成空、
// 同一个键写两遍。
//
// 值的含义不住在这个包里。哪个键必须为正、0 又是什么语义、95 天是怎么推出来的——
// 那些判断住在各功能包自己的 deploy.go 里（校验与语义同住，改代码的人不会只看到
// 一个陌生的键名）。本包只管形状：时长怎么写、哪些键名是总配置专用的。
//
// 用 YAML 而不是 JSON 是为了让注释就是注释（写在值的上一行或同一行），而不是伪造成
// 一个叫 doc 的对象。注释只被收集、被 Dump 打进启动日志，不参与校验：少一句注释不
// 影响程序怎么跑，拿"起不来"去逼它等于把写作规范提到配置正确性的档位上。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Dur 是配置文件里的时长。写法是 time.ParseDuration 认识的字符串：YAML 里
// `warm: 5m` 就是字符串，不用引号。
//
// 不接受数字：`warm: 300000000000` 一旦合法，同一个意思就有两种落法，而人只会挑
// 当天顺手的那种写——格式本身就成了第二个漂移源。
type Dur struct{ time.Duration }

func (d *Dur) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag != "!!str" {
		return fmt.Errorf("时长要写成字符串（如 5m、17h），第 %d 行给的是 %s", node.Line, node.Tag)
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("时长 %q 看不懂: %w", node.Value, err)
	}
	d.Duration = parsed
	return nil
}

func (d Dur) MarshalYAML() (any, error) { return d.Duration.String(), nil }

// reserved 是总配置文件专用的键：它们说的是"这台机器"，不属于任何功能。
// 功能配置文件里出现同名键一律拒绝加载——总配置与功能配置各写着一个 interval
// 式的东西，就没有唯一源可言了。
var reserved = map[string]bool{
	"creds": true, "db": true, "chrome": true, "tz": true,
	"api": true, "source": true, "admin": true,
}

// File 是一次加载的产物：已填好值的结构体，加上每个键旁边那句注释。
// Dump 要的是它，而不是再去读一遍文件——日志里的值因此必然就是进程真正吃进去的值。
type File struct {
	Path     string
	target   any
	keys     []string
	comments map[string]string
}

// Load 读一个功能的配置文件（config/<功能>.yml），解进 target。
// target 必须是*扁平*结构体；功能文件还多一道：不许出现总配置专用的键名。
func Load(path string, target any) (*File, error) {
	f, err := load(path, target)
	if err != nil {
		return nil, err
	}
	for _, k := range f.keys {
		if reserved[k] {
			return nil, fmt.Errorf("%s 里不该出现 %q：它是总配置文件的键，不属于这个功能", path, k)
		}
	}
	return f, nil
}

// Run 是总配置文件 config.yml 的形状。只装机器级事实，一个功能名都不出现。
type Run struct {
	Creds  string   `yaml:"creds"`  // 凭据文件路径；凭据内容绝不进配置文件
	DB     string   `yaml:"db"`     // bbolt 文件；海报缓存目录由它的父目录派生
	Chrome string   `yaml:"chrome"` // 允许空串：留空 = 不出图
	TZ     string   `yaml:"tz"`
	API    string   `yaml:"api"`
	Source string   `yaml:"source"`
	Admin  []string `yaml:"admin"`
}

// LoadRun 读总配置并校验机器级字段的必填项。
// chrome 是唯一允许为空的字符串——"留空 = 不启用出图"是它定义出来的功能语义，
// 不是没填；把它当缺值报错就等于把这条合法路径杀了。
func LoadRun(path string) (Run, *File, error) {
	var r Run
	f, err := load(path, &r)
	if err != nil {
		return Run{}, nil, err
	}
	var bad Bad
	bad.NonEmpty("creds", r.Creds)
	bad.NonEmpty("db", r.DB)
	bad.NonEmpty("tz", r.TZ)
	bad.NonEmpty("api", r.API)
	bad.NonEmpty("source", r.Source)
	r.Admin = CleanList(r.Admin)
	if err := bad.Err(path); err != nil {
		return Run{}, nil, err
	}
	return r, f, nil
}

func load(path string, target any) (*File, error) {
	// 先验形状再解码：yaml.v3 拿到非指针 target 不会报错，会在 reflect.Value.Set 上
	// panic 出去（它把非类型错误原样重抛），所以这道检查必须排在它前面。
	params, err := paramKeys(target)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读配置文件失败: %w", err)
	}
	// 第一遍：严格解进结构体。KnownFields 让拼错的键当场报，且一次报全；
	// 解进结构体这条路还会把重复键当错误（Node 那条路不会，见下）。
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(target); err != nil {
		return nil, fmt.Errorf("%s 读不通: %w", path, err)
	}
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s 里有多余的文档（每个文件只放一个 YAML 文档）", path)
	}
	// 第二遍：只为拿注释。yaml.v3 把注释留在 Node 上，所以注释能与键逐一对账。
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%s 读不通: %w", path, err)
	}
	f := &File{Path: path, target: target, comments: map[string]string{}}
	if err := f.scanTop(&root, params); err != nil {
		return nil, err
	}
	return f, nil
}

// scanTop 走一遍顶层映射：把每个键的注释收下来给 Dump 用，并查出"结构体声明了
// 这个键、文件里却没有"这种情形。
//
// 这一遍解析不是为了注释：解进结构体分不出"没写这个键"与"写了 0"，而这两种情形必须
// 能区分——少了 warm 与 warm 是 0 都会让 time.NewTicker 炸掉，但只有前者说明文件写坏了。
// 注释有没有写全不在这里管：那是一句给人看的话，缺了不影响程序跑，用"起不来"去逼它
// 等于把写作规范提到配置正确性的档位上。
//
// 重复键也不在这里查：解进结构体那一路（yaml.v3）原生就报 "already defined"。
func (f *File) scanTop(root *yaml.Node, params []string) error {
	doc := root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return fmt.Errorf("%s 顶层必须是键值对", f.Path)
	}
	seen := make(map[string]bool, len(doc.Content)/2)
	for i := 0; i+1 < len(doc.Content); i += 2 {
		k, v := doc.Content[i], doc.Content[i+1]
		key := k.Value
		seen[key] = true
		f.keys = append(f.keys, key)

		// 空值要单独拦一道：`interval:` 后面什么都不写，YAML 判它 null，而 yaml.v3
		// 对 null 节点根本不调自定义解码 —— 于是 0 会冒充"已配置"一路混到
		// time.NewTicker(0) 那行才炸。留空要么写 ""、要么写 []，让"这里确实想留空"
		// 成为一个看得见的决定。
		if v.Tag == "!!null" {
			return fmt.Errorf("%s 第 %d 行：%q 没有给值。确实要留空就写 \"\"（列表写 []）", f.Path, k.Line, key)
		}
		note := cleanComment(k.HeadComment)
		if note == "" {
			note = cleanComment(v.LineComment)
		}
		f.comments[key] = note
	}
	for _, k := range params {
		if !seen[k] {
			return fmt.Errorf("%s 里少了 %q：结构体声明了它，文件就必须给出值（没有默认值可退）", f.Path, k)
		}
	}
	return nil
}

// cleanComment 把 YAML 原样留在节点上的注释剥成纯文本：逐行去掉前导 #，再收空白。
// 不剥的话注释里那串 "# " 会长在启动日志的每行上。
func cleanComment(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "#"))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

var unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()

// paramKeys 反射出结构体声明的键（按字段声明顺序）。
func paramKeys(target any) ([]string, error) {
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return nil, errors.New("配置结构体必须是 *struct")
	}
	t := v.Elem().Type()
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		if name == "-" {
			continue
		}
		if f.Type.Kind() == reflect.Struct && !implementsLeaf(f.Type) {
			// 只支持扁平结构体：嵌套一层的话"文件里的键"与结构体字段就得两层对账，
			// 而这整套机制的卖点正是"一眼对得上的双向相等"。
			return nil, fmt.Errorf("字段 %s 是嵌套结构体，配置文件只支持扁平结构", name)
		}
		keys = append(keys, name)
	}
	return keys, nil
}

func implementsLeaf(t reflect.Type) bool {
	return t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType)
}

// Bad 攒校验失败项，最后一次性报出来。改配置的人一次就该看到全部问题，
// 而不是修一个键、重跑、再看见下一个。
type Bad struct{ msgs []string }

func (b *Bad) Pos(key string, d time.Duration) {
	if d <= 0 {
		b.msgs = append(b.msgs, fmt.Sprintf("%s 必须是正数，现在是 %s", key, d))
	}
}

func (b *Bad) PosInt(key string, n int) {
	if n <= 0 {
		b.msgs = append(b.msgs, fmt.Sprintf("%s 必须至少是 1，现在是 %d", key, n))
	}
}

func (b *Bad) NonEmpty(key, s string) {
	if strings.TrimSpace(s) == "" {
		b.msgs = append(b.msgs, fmt.Sprintf("%s 不能留空", key))
	}
}

func (b *Bad) Other(err error) { b.msgs = append(b.msgs, err.Error()) }

func (b *Bad) Err(path string) error {
	if len(b.msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s 的参数不合用：%s", path, strings.Join(b.msgs, "；"))
}

// CleanList 逐项 TrimSpace 并过滤跳过空项。
// 手改配置文件或模板渲染时偶尔产生的空行/空格项直接清洗丢弃，避免无害空白阻断进程启动。
func CleanList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// Dump 把一个已加载的文件打成 "文件名.键 = 值｜注释第一句"。
// 调用点要排在读凭据之前，否则"值搬对了"这件事得有一份能连上平台的凭据才验得了；
// 逐文件打一遍，就是"开七个文件才能看全参数"那份代价的运行时补偿。
func (f *File) Dump(logf func(format string, args ...any)) {
	label := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
	v := reflect.ValueOf(f.target).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		if name == "-" {
			continue
		}
		line := fmt.Sprintf("%s.%s = %s", label, name, formatValue(v.Field(i)))
		if note := f.comments[name]; note != "" {
			line += "｜" + firstClause(note)
		}
		logf("%s", line)
	}
}

func formatValue(fv reflect.Value) string {
	switch v := fv.Interface().(type) {
	case Dur:
		return v.Duration.String()
	case time.Duration:
		return v.String()
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case []string:
		return strings.Join(v, ",")
	}
	return fmt.Sprintf("%v", fv.Interface())
}

// firstClause 只取注释的第一行/第一句：日志里堆几十个字就没法逐行核了，
// 全文在配置文件里，改的人本来就要打开它。
func firstClause(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	for _, sep := range []string{"。", "；"} {
		if i := strings.Index(s, sep); i > 0 {
			return s[:i]
		}
	}
	return s
}
