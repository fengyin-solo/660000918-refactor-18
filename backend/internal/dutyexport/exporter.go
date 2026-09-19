// Package dutyexport 统一值班资料导出的全部口径。
//
// 之前筛选、文件组装、归档摘要、恢复判断各写一遍，新增一种资料类型需要在
// 多处同步修改，且容易出现摘要与归档内容不一致。这里把每种资料类型收拢为
// 一个 MaterialType 定义（注册表中的唯一事实来源），导出 / 归档 / 恢复 /
// 历史回看全部走 buildOne 这一条管线：
//
//	Filter（按值班日时间窗取数） -> 排序 -> Assemble（组装文件） -> 摘要推导
//
// 新增资料类型时，只需实现 MaterialType 并调用 Register，流程代码无需改动。
package dutyexport

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const dateLayout = "2006-01-02"

// BuildContext 是一次资料构建的固定上下文，筛选与组装共用。
type BuildContext struct {
	Date     string         // 值班日，格式 YYYY-MM-DD
	From, To time.Time      // 值班日对应的 [From, To) 半开时间窗
	Location *time.Location // 时间窗按该时区划定
	Key      string         // 资料类型标识
	Name     string         // 资料类型展示名
}

// Item 是各资料类型筛选后交给组装环节的原始记录。
type Item struct {
	Type      string    // 资料类型标识
	ID        string    // 记录唯一标识
	Timestamp time.Time // 业务时间，用于时间窗判断与排序
	// Title 是该记录的摘要行。文件内容与归档摘要由同一份 Title 列表推导，
	// 从根上杜绝"摘要与归档内容对不上"。
	Title  string
	Detail string // 可选的正文补充
}

// File 是组装产物。归档存的是它，恢复返回的也是它，全程同一份字节。
type File struct {
	Name        string
	ContentType string
	Content     []byte
}

// MaterialType 描述一种值班资料的完整口径。新增资料类型只需实现本接口。
type MaterialType interface {
	Key() string
	DisplayName() string
	// Filter 按 [from, to) 半开区间取数（含 from、不含 to）。
	Filter(ctx *BuildContext, from, to time.Time) ([]Item, error)
	// Assemble 把筛选结果组装为文件；items 已按时间、ID 排好序。
	Assemble(ctx *BuildContext, from, to time.Time, items []Item) (File, error)
}

// QueryFunc 是 TextMaterial 的取数函数签名。
type QueryFunc func(from, to time.Time) ([]Item, error)

// TextMaterial 是基于纯文本文件的通用资料类型定义：使用方只需提供
// Key、展示名与一个按时间窗取数的 Query，组装规则由包内统一提供。
type TextMaterial struct {
	TypeKey  string
	TypeName string
	Query    QueryFunc
}

func (m TextMaterial) Key() string         { return m.TypeKey }
func (m TextMaterial) DisplayName() string { return m.TypeName }

func (m TextMaterial) Filter(_ *BuildContext, from, to time.Time) ([]Item, error) {
	if m.Query == nil {
		return nil, nil
	}
	return m.Query(from, to)
}

// Assemble 使用统一的文本格式，保证所有资料类型的文件结构一致、可复现。
func (m TextMaterial) Assemble(ctx *BuildContext, from, to time.Time, items []Item) (File, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", ctx.Name)
	fmt.Fprintf(&b, "统计区间: %s ~ %s\n", from.Format(time.RFC3339), to.Format(time.RFC3339))
	fmt.Fprintf(&b, "记录数量: %d\n", len(items))
	fmt.Fprintln(&b, strings.Repeat("-", 40))
	if len(items) == 0 {
		fmt.Fprintln(&b, "本时段无记录")
	}
	for _, it := range items {
		fmt.Fprintf(&b, "[%s] %s | %s\n", it.Timestamp.Format(time.RFC3339), it.ID, it.Title)
		if it.Detail != "" {
			fmt.Fprintf(&b, "    %s\n", strings.ReplaceAll(it.Detail, "\n", "\n    "))
		}
	}
	return File{
		Name:        ctx.Date + "-" + ctx.Key + ".txt",
		ContentType: "text/plain; charset=utf-8",
		Content:     []byte(b.String()),
	}, nil
}

// MaterialView 是一份资料在任意环节（导出/恢复/历史）的统一视图。
type MaterialView struct {
	Type         string   `json:"type"`
	Name         string   `json:"name"`
	FileName     string   `json:"fileName"`
	ItemCount    int      `json:"itemCount"`
	Missing      bool     `json:"missing"`      // 该值班日是否无任何资料
	Archived     bool     `json:"archived"`     // 是否来自归档
	SummaryLines []string `json:"summaryLines"` // 与文件内容同源的摘要行
	File         File     `json:"-"`
}

// Bundle 是一次导出（按值班日）的完整结果。
type Bundle struct {
	Date      string
	GeneratedAt time.Time
	Materials []MaterialView
	Summary   string // 规范摘要文本
}

// ArchiveRecord 是一条不可变归档记录。
type ArchiveRecord struct {
	Date       string
	Type       string
	Name       string
	FileName   string
	ItemCount  int
	Missing    bool
	Lines      []string
	File       File
	ArchivedAt time.Time // 归档时刻，之后永不改变
}

// Exporter 持有资料类型注册表与归档存储，是统一入口。
type Exporter struct {
	loc *time.Location

	mu       sync.RWMutex
	registry []MaterialType            // 按注册顺序，导出顺序稳定
	index    map[string]MaterialType   // key -> spec
	archive  map[string]*ArchiveRecord // date/type -> record
	clock    func() time.Time
}

// New 创建 Exporter，loc 为 nil 时使用本地时区。
func New(loc *time.Location) *Exporter {
	if loc == nil {
		loc = time.Local
	}
	return &Exporter{
		loc:     loc,
		index:   map[string]MaterialType{},
		archive: map[string]*ArchiveRecord{},
		clock:   time.Now,
	}
}

// Register 注册一种资料类型；重复注册返回错误，避免口径被静默覆盖。
func (e *Exporter) Register(spec MaterialType) error {
	if spec == nil || spec.Key() == "" {
		return errors.New("dutyexport: material type and key must not be empty")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.index[spec.Key()]; ok {
		return fmt.Errorf("dutyexport: material type %q already registered", spec.Key())
	}
	e.index[spec.Key()] = spec
	e.registry = append(e.registry, spec)
	return nil
}

// RegisteredTypes 按注册顺序返回已注册类型。
func (e *Exporter) RegisteredTypes() []MaterialType {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]MaterialType, len(e.registry))
	copy(out, e.registry)
	return out
}

func (e *Exporter) resolve(keys []string) ([]MaterialType, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(keys) == 0 {
		out := make([]MaterialType, len(e.registry))
		copy(out, e.registry)
		return out, nil
	}
	specs := make([]MaterialType, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		spec, ok := e.index[k]
		if !ok {
			return nil, fmt.Errorf("dutyexport: unknown material type %q", k)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func (e *Exporter) dayWindow(date string) (time.Time, time.Time, error) {
	day, err := time.ParseInLocation(dateLayout, date, e.loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("dutyexport: invalid date %q, want YYYY-MM-DD", date)
	}
	return day, day.AddDate(0, 0, 1), nil
}

func archiveKey(date, typ string) string { return date + "/" + typ }

// buildOne 是唯一的构建管线：时间筛选 -> 排序 -> 文件组装 -> 摘要推导。
// 导出、归档、恢复（无归档时的兜底）都走这里。
func (e *Exporter) buildOne(spec MaterialType, date string) (MaterialView, error) {
	from, to, err := e.dayWindow(date)
	if err != nil {
		return MaterialView{}, err
	}
	ctx := &BuildContext{
		Date: date, From: from, To: to, Location: e.loc,
		Key: spec.Key(), Name: spec.DisplayName(),
	}
	items, err := spec.Filter(ctx, from, to)
	if err != nil {
		return MaterialView{}, fmt.Errorf("dutyexport: filter %q: %w", spec.Key(), err)
	}
	// 统一过滤 + 稳定排序，各类型不必各自实现，口径只有一份。
	filtered := items[:0:0]
	for _, it := range items {
		if it.Type == "" {
			it.Type = spec.Key()
		}
		if !it.Timestamp.Before(from) && it.Timestamp.Before(to) {
			filtered = append(filtered, it)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].Timestamp.Equal(filtered[j].Timestamp) {
			return filtered[i].Timestamp.Before(filtered[j].Timestamp)
		}
		return filtered[i].ID < filtered[j].ID
	})

	file, err := spec.Assemble(ctx, from, to, filtered)
	if err != nil {
		return MaterialView{}, fmt.Errorf("dutyexport: assemble %q: %w", spec.Key(), err)
	}

	lines := make([]string, len(filtered))
	for i, it := range filtered {
		lines[i] = fmt.Sprintf("[%s] %s | %s", it.Timestamp.Format(time.RFC3339), it.ID, it.Title)
	}
	return MaterialView{
		Type:         spec.Key(),
		Name:         spec.DisplayName(),
		FileName:     file.Name,
		ItemCount:    len(filtered),
		Missing:      len(filtered) == 0,
		SummaryLines: lines,
		File:         file,
	}, nil
}

// ExportBundle 按值班日导出资料。keys 为空表示导出全部已注册类型。
// 只读取、组装，不写归档。
func (e *Exporter) ExportBundle(date string, keys []string) (*Bundle, error) {
	specs, err := e.resolve(keys)
	if err != nil {
		return nil, err
	}
	bundle := &Bundle{Date: date, GeneratedAt: e.clock()}
	for _, spec := range specs {
		view, err := e.buildOne(spec, date)
		if err != nil {
			return nil, err
		}
		bundle.Materials = append(bundle.Materials, view)
	}
	bundle.Summary = buildSummary(date, bundle.Materials)
	return bundle, nil
}

func buildSummary(date string, views []MaterialView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "值班资料汇总 %s\n", date)
	for _, v := range views {
		if v.Missing {
			fmt.Fprintf(&b, "- %s: 资料缺失\n", v.Name)
		} else {
			fmt.Fprintf(&b, "- %s: %d 条\n", v.Name, v.ItemCount)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ArchiveOutcome 描述单个类型的归档结果。
type ArchiveOutcome struct {
	Type            string
	Archived        bool // 本次是否新写入
	AlreadyArchived bool // 此前已归档，本次原样保留
	SkippedMissing  bool // 资料缺失，不产生归档
	Record          *ArchiveRecord
}

// Archive 按值班日归档全部已注册类型。
// 已归档的记录一律原样保留（已归档数据不能被重写）；资料缺失的类型跳过，
// 待资料补齐后再次归档才会写入。
func (e *Exporter) Archive(date string) ([]ArchiveOutcome, error) {
	specs, err := e.resolve(nil)
	if err != nil {
		return nil, err
	}
	outcomes := make([]ArchiveOutcome, 0, len(specs))
	for _, spec := range specs {
		key := archiveKey(date, spec.Key())

		e.mu.RLock()
		existing := e.archive[key]
		e.mu.RUnlock()
		if existing != nil {
			outcomes = append(outcomes, ArchiveOutcome{
				Type: spec.Key(), AlreadyArchived: true, Record: cloneRecord(existing),
			})
			continue
		}

		view, err := e.buildOne(spec, date)
		if err != nil {
			return nil, err
		}
		if view.Missing {
			outcomes = append(outcomes, ArchiveOutcome{Type: spec.Key(), SkippedMissing: true})
			continue
		}

		record := &ArchiveRecord{
			Date:       date,
			Type:       view.Type,
			Name:       view.Name,
			FileName:   view.FileName,
			ItemCount:  view.ItemCount,
			Missing:    false,
			Lines:      append([]string(nil), view.SummaryLines...),
			File:       cloneFile(view.File),
			ArchivedAt: e.clock(),
		}

		e.mu.Lock()
		// 双检：并发归档时后到者也不得覆盖先写入的记录。
		if current := e.archive[key]; current != nil {
			e.mu.Unlock()
			outcomes = append(outcomes, ArchiveOutcome{
				Type: spec.Key(), AlreadyArchived: true, Record: cloneRecord(current),
			})
			continue
		}
		e.archive[key] = record
		e.mu.Unlock()

		outcomes = append(outcomes, ArchiveOutcome{
			Type: spec.Key(), Archived: true, Record: cloneRecord(record),
		})
	}
	return outcomes, nil
}

// Restore 恢复某值班日的某类资料：有归档则永远返回归档原件（可重复恢复、
// 结果逐字节一致，且不受数据源后续变化影响）；无归档时按当前数据实时组装
// 一份临时结果（Archived=false），缺失时 Missing=true，两者都不写归档。
func (e *Exporter) Restore(date, typ string) (MaterialView, bool, error) {
	specs, err := e.resolve([]string{typ})
	if err != nil {
		return MaterialView{}, false, err
	}
	spec := specs[0]

	e.mu.RLock()
	record := e.archive[archiveKey(date, typ)]
	e.mu.RUnlock()
	if record != nil {
		return recordView(record), true, nil
	}

	view, err := e.buildOne(spec, date)
	if err != nil {
		return MaterialView{}, false, err
	}
	return view, false, nil
}

// History 回看某值班日已归档资料，按类型排序，只读取归档、绝不触发重建，
// 因此数据源的后续变化不会影响历史记录。
func (e *Exporter) History(date string) ([]*ArchiveRecord, error) {
	if _, _, err := e.dayWindow(date); err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []*ArchiveRecord
	for _, rec := range e.archive {
		if rec.Date == date {
			out = append(out, cloneRecord(rec))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out, nil
}

func recordView(r *ArchiveRecord) MaterialView {
	return MaterialView{
		Type:         r.Type,
		Name:         r.Name,
		FileName:     r.FileName,
		ItemCount:    r.ItemCount,
		Missing:      r.Missing,
		Archived:     true,
		SummaryLines: append([]string(nil), r.Lines...),
		File:         cloneFile(r.File),
	}
}

func cloneFile(f File) File {
	return File{
		Name:        f.Name,
		ContentType: f.ContentType,
		Content:     append([]byte(nil), f.Content...),
	}
}

func cloneRecord(r *ArchiveRecord) *ArchiveRecord {
	cp := *r
	cp.Lines = append([]string(nil), r.Lines...)
	cp.File = cloneFile(r.File)
	return &cp
}
