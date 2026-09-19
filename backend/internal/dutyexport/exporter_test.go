package dutyexport

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const testDate = "2026-09-19"

func testExporter(t *testing.T) (*Exporter, *ItemStore, time.Time) {
	t.Helper()
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, loc)
	store := NewItemStore()
	exp := New(loc)
	exp.clock = func() time.Time { return now }
	if err := RegisterDefault(exp, store); err != nil {
		t.Fatalf("register defaults: %v", err)
	}
	return exp, store, now
}

func at(day, hh, mm string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hh+":"+mm, time.FixedZone("CST", 8*3600))
	if err != nil {
		panic(err)
	}
	return t
}

func seedAlerts(store *ItemStore) {
	store.Add(Item{Type: TypeAlert, ID: "a1", Timestamp: at(testDate, "02:00"), Title: "低电量告警"})
	store.Add(Item{Type: TypeAlert, ID: "a2", Timestamp: at(testDate, "08:30"), Title: "进入危险区域"})
	store.Add(Item{Type: TypeAlert, ID: "a0", Timestamp: at("2026-09-18", "23:59"), Title: "前一天告警"})
	store.Add(Item{Type: TypeAlert, ID: "a3", Timestamp: at("2026-09-20", "00:00"), Title: "次日零点告警"})
}

// 路径一：按时间筛选 —— 只保留值班日 [00:00, 次日00:00) 的记录，并按时间排序。
func TestTimeFilterByDutyDay(t *testing.T) {
	exp, store, _ := testExporter(t)
	seedAlerts(store)

	bundle, err := exp.ExportBundle(testDate, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var alerts *MaterialView
	for i := range bundle.Materials {
		if bundle.Materials[i].Type == TypeAlert {
			alerts = &bundle.Materials[i]
		}
	}
	if alerts == nil {
		t.Fatal("alerts material missing from bundle")
	}
	if alerts.ItemCount != 2 {
		t.Fatalf("want 2 alerts in day window, got %d", alerts.ItemCount)
	}
	if alerts.SummaryLines[0] == alerts.SummaryLines[1] ||
		!strings.Contains(alerts.SummaryLines[0], "a1") ||
		!strings.Contains(alerts.SummaryLines[1], "a2") {
		t.Fatalf("unexpected ordering: %v", alerts.SummaryLines)
	}
	content := string(alerts.File.Content)
	if !strings.Contains(content, "a1") || !strings.Contains(content, "a2") ||
		strings.Contains(content, "a0") || strings.Contains(content, "a3") {
		t.Fatalf("file content not consistent with filter: %q", content)
	}

	// 边界：恰好落在窗口起点计入，恰好落在终点（次日零点）不计入。
	otherDate := "2026-09-20"
	ob, _ := exp.ExportBundle(otherDate, []string{TypeAlert})
	v := ob.Materials[0]
	if v.ItemCount != 1 || !strings.Contains(v.SummaryLines[0], "a3") {
		t.Fatalf("half-open boundary broken: %v", v.SummaryLines)
	}
}

// 筛选口径即使在数据源越界返回时，也由管线统一兜底，不允许越窗数据混入。
func TestFilterEnforcedEvenIfSpecMisbehaves(t *testing.T) {
	exp, _, _ := testExporter(t)
	exp.Register(TextMaterial{
		TypeKey: "loose", TypeName: "越界取数",
		Query: func(from, to time.Time) ([]Item, error) {
			return []Item{{ID: "x1", Timestamp: from.Add(-time.Hour), Title: "窗外"}}, nil
		},
	})
	b, err := exp.ExportBundle(testDate, []string{"loose"})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Materials[0].Missing || b.Materials[0].ItemCount != 0 {
		t.Fatalf("out-of-window item must be clamped, got %+v", b.Materials[0])
	}
}

// 路径二：部分资料缺失 —— 导出与摘要照常，缺失类型跳过归档；资料补齐后再归档可补入。
func TestPartialMissing(t *testing.T) {
	exp, store, _ := testExporter(t)
	seedAlerts(store) // 只有告警，没有巡检

	bundle, err := exp.ExportBundle(testDate, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(bundle.Summary, "告警记录: 2 条") ||
		!strings.Contains(bundle.Summary, "巡检记录: 资料缺失") {
		t.Fatalf("summary mismatch: %q", bundle.Summary)
	}

	outcomes, err := exp.Archive(testDate)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	got := map[string]ArchiveOutcome{}
	for _, o := range outcomes {
		got[o.Type] = o
	}
	if !got[TypeAlert].Archived || got[TypeAlert].SkippedMissing {
		t.Fatalf("alerts should archive: %+v", got[TypeAlert])
	}
	if !got[TypePatrol].SkippedMissing || got[TypePatrol].Record != nil {
		t.Fatalf("empty patrol should be skipped: %+v", got[TypePatrol])
	}

	// 补齐巡检资料后再次归档：告警原样保留，巡检补写成功。
	store.Add(Item{Type: TypePatrol, ID: "p1", Timestamp: at(testDate, "10:00"), Title: "例行巡检"})
	outcomes, _ = exp.Archive(testDate)
	for _, o := range outcomes {
		switch o.Type {
		case TypeAlert:
			if !o.AlreadyArchived {
				t.Fatal("alerts must remain already-archived")
			}
		case TypePatrol:
			if !o.Archived || o.Record == nil || o.Record.ItemCount != 1 {
				t.Fatalf("patrol should now archive: %+v", o)
			}
		}
	}
}

// 路径三：重复恢复 —— 多次恢复逐字节一致；归档后数据源变化不影响恢复结果；
// 未归档类型恢复到的是实时组装结果且不产生归档。
func TestRepeatedRestore(t *testing.T) {
	exp, store, _ := testExporter(t)
	seedAlerts(store)

	if _, err := exp.Archive(testDate); err != nil {
		t.Fatalf("archive: %v", err)
	}

	v1, archived1, err := exp.Restore(testDate, TypeAlert)
	if err != nil || !archived1 {
		t.Fatalf("first restore: archived=%v err=%v", archived1, err)
	}
	v2, archived2, err := exp.Restore(testDate, TypeAlert)
	if err != nil || !archived2 {
		t.Fatalf("second restore: archived=%v err=%v", archived2, err)
	}
	if !bytes.Equal(v1.File.Content, v2.File.Content) ||
		v1.FileName != v2.FileName || v1.ItemCount != v2.ItemCount {
		t.Fatal("repeated restore must be byte-identical")
	}

	// 归档之后业务数据继续变化，恢复结果必须仍是归档原件。
	store.Add(Item{Type: TypeAlert, ID: "a9", Timestamp: at(testDate, "23:00"), Title: "归档后新增告警"})
	v3, _, _ := exp.Restore(testDate, TypeAlert)
	if !bytes.Equal(v3.File.Content, v1.File.Content) {
		t.Fatal("restore content changed unexpectedly")
	}
	if v3.ItemCount != 2 || strings.Contains(string(v3.File.Content), "a9") {
		t.Fatalf("restore leaked post-archive data: %q", v3.File.Content)
	}

	// 从未归档的交接班类型：恢复实时结果（含缺失标记），不落归档。
	hv, archived, err := exp.Restore(testDate, TypeHandover)
	if err != nil || archived {
		t.Fatalf("unarchived restore: archived=%v err=%v", archived, err)
	}
	if !hv.Missing {
		t.Fatal("handovers should be missing on that day")
	}
	if recs, _ := exp.History(testDate); len(recs) != 1 {
		t.Fatalf("live restore must not create archive, history=%d", len(recs))
	}
}

// 路径四：历史记录回看 —— 只看已归档内容；归档后新增数据、再次归档都不改变历史。
func TestHistoryReview(t *testing.T) {
	exp, store, _ := testExporter(t)
	seedAlerts(store)

	recs, err := exp.History(testDate)
	if err != nil || len(recs) != 0 {
		t.Fatalf("empty history expected, got %v err=%v", len(recs), err)
	}

	if _, err := exp.Archive(testDate); err != nil {
		t.Fatalf("archive: %v", err)
	}
	recs, _ = exp.History(testDate)
	if len(recs) != 1 || recs[0].Type != TypeAlert || recs[0].ItemCount != 2 {
		t.Fatalf("history after archive: %+v", recs)
	}
	snapshot := append([]byte(nil), recs[0].File.Content...)
	archivedAt := recs[0].ArchivedAt

	// 数据源变化 + 再次归档，历史记录仍是首次归档的原件与时间戳。
	store.Add(Item{Type: TypeAlert, ID: "a9", Timestamp: at(testDate, "23:30"), Title: "归档后新增"})
	if _, err := exp.Archive(testDate); err != nil {
		t.Fatalf("re-archive: %v", err)
	}
	recs, _ = exp.History(testDate)
	if len(recs) != 1 {
		t.Fatalf("history must keep one record, got %d", len(recs))
	}
	if !bytes.Equal(recs[0].File.Content, snapshot) {
		t.Fatal("archived history content was rewritten")
	}
	if !recs[0].ArchivedAt.Equal(archivedAt) {
		t.Fatal("archived_at was rewritten")
	}
	if strings.Contains(string(recs[0].File.Content), "a9") || recs[0].ItemCount != 2 {
		t.Fatal("post-archive data leaked into history")
	}

	// 实时导出能看到新数据，而历史保持旧貌，两者口径互不污染。
	live, _ := exp.ExportBundle(testDate, []string{TypeAlert})
	if live.Materials[0].ItemCount != 3 {
		t.Fatalf("live export should see 3, got %d", live.Materials[0].ItemCount)
	}
}

// 归档记录不可重写：已存在的归档跳过写入，时钟推进也不改变 ArchivedAt。
func TestArchiveNeverRewrites(t *testing.T) {
	exp, store, now := testExporter(t)
	seedAlerts(store)

	first, err := exp.Archive(testDate)
	if err != nil {
		t.Fatal(err)
	}
	if !first[0].Archived {
		t.Fatal("first archive should write")
	}

	exp.clock = func() time.Time { return now.Add(2 * time.Hour) }
	store.Add(Item{Type: TypeAlert, ID: "a9", Timestamp: at(testDate, "23:00"), Title: "新增"})
	second, err := exp.Archive(testDate)
	if err != nil {
		t.Fatal(err)
	}
	if !second[0].AlreadyArchived || second[0].Archived {
		t.Fatalf("existing archive must be preserved: %+v", second[0])
	}
	if !second[0].Record.ArchivedAt.Equal(now) {
		t.Fatalf("ArchivedAt moved: %v", second[0].Record.ArchivedAt)
	}
	if strings.Contains(string(second[0].Record.File.Content), "a9") {
		t.Fatal("rewritten archive contains new data")
	}
}

// 摘要与归档文件同源：每条摘要行都必须能在归档文件正文中找到。
func TestSummaryMatchesArchivedContent(t *testing.T) {
	exp, store, _ := testExporter(t)
	seedAlerts(store)

	outcomes, err := exp.Archive(testDate)
	if err != nil {
		t.Fatal(err)
	}
	rec := outcomes[0].Record
	if rec == nil {
		t.Fatal("alert record missing")
	}
	content := string(rec.File.Content)
	if len(rec.Lines) != rec.ItemCount {
		t.Fatalf("lines/count mismatch: %d vs %d", len(rec.Lines), rec.ItemCount)
	}
	for _, line := range rec.Lines {
		if !strings.Contains(content, line) {
			t.Fatalf("summary line not present in archived file: %q", line)
		}
	}

	v, archived, _ := exp.Restore(testDate, TypeAlert)
	if !archived || len(v.SummaryLines) != len(rec.Lines) {
		t.Fatal("restore summary diverged from archive")
	}
	for i := range rec.Lines {
		if v.SummaryLines[i] != rec.Lines[i] {
			t.Fatalf("line %d diverged: %q vs %q", i, v.SummaryLines[i], rec.Lines[i])
		}
	}
}

// 扩展新资料类型只维护一份口径：实现 MaterialType 注册一次，
// 筛选/组装/归档/恢复/历史全链路自动生效，流程代码零改动。
func TestNewTypeSingleRegistration(t *testing.T) {
	exp, store, _ := testExporter(t)
	seedAlerts(store)

	custom := TextMaterial{
		TypeKey:  "weather",
		TypeName: "气象记录",
		Query:    store.Query("weather"),
	}
	if err := exp.Register(custom); err != nil {
		t.Fatal(err)
	}
	if err := exp.Register(custom); err == nil {
		t.Fatal("duplicate registration must fail")
	}
	store.Add(Item{Type: "weather", ID: "w1", Timestamp: at(testDate, "12:00"), Title: "晴 26℃"})

	bundle, err := exp.ExportBundle(testDate, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range bundle.Materials {
		if m.Type == "weather" {
			found = true
			if m.ItemCount != 1 || !strings.Contains(string(m.File.Content), "晴 26℃") {
				t.Fatalf("custom type pipeline broken: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("registered custom type absent from bundle")
	}

	if _, err := exp.Archive(testDate); err != nil {
		t.Fatal(err)
	}
	v, archived, err := exp.Restore(testDate, "weather")
	if err != nil || !archived || v.ItemCount != 1 {
		t.Fatalf("custom type restore: archived=%v err=%v", archived, err)
	}
	recs, _ := exp.History(testDate)
	keys := map[string]bool{}
	for _, r := range recs {
		keys[r.Type] = true
	}
	if !keys["weather"] || !keys[TypeAlert] {
		t.Fatalf("history missing types: %v", keys)
	}
}

func TestInvalidInputs(t *testing.T) {
	exp, _, _ := testExporter(t)

	if _, err := exp.ExportBundle("2026/09/19", nil); err == nil {
		t.Fatal("invalid date must error")
	}
	if _, err := exp.ExportBundle(testDate, []string{"nope"}); err == nil {
		t.Fatal("unknown type must error")
	}
	if _, _, err := exp.Restore(testDate, "nope"); err == nil {
		t.Fatal("restore unknown type must error")
	}
	if _, err := exp.History("not-a-date"); err == nil {
		t.Fatal("history invalid date must error")
	}
	if err := exp.Register(TextMaterial{}); err == nil {
		t.Fatal("empty key registration must error")
	}
}
