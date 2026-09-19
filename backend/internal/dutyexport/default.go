package dutyexport

import "time"

// ItemStore 是内置资料类型共用的内存数据源。真实项目中可替换为数据库实现，
// 只要为每种资料类型提供同样形态的 QueryFunc 即可，导出管线无需改动。
type ItemStore struct {
	items []Item
}

// NewItemStore 创建数据源并写入初始记录。
func NewItemStore(initial ...Item) *ItemStore {
	s := &ItemStore{}
	s.items = append(s.items, initial...)
	return s
}

// Add 追加一条记录（用于模拟业务系统持续产生数据）。
func (s *ItemStore) Add(it Item) {
	s.items = append(s.items, it)
}

// Query 返回 [from, to) 区间内指定类型的记录。注意这里只做粗筛，
// 精确的时间窗过滤与排序由 Exporter.buildOne 统一完成，口径不分散。
func (s *ItemStore) Query(typ string) QueryFunc {
	return func(from, to time.Time) ([]Item, error) {
		var out []Item
		for _, it := range s.items {
			if it.Type == typ && !it.Timestamp.Before(from) && it.Timestamp.Before(to) {
				out = append(out, it)
			}
		}
		return out, nil
	}
}

// 内置值班资料类型标识。新增类型时在此声明 key 并通过 RegisterDefault 注册即可。
const (
	TypeAlert     = "alerts"    // 告警记录
	TypePatrol    = "patrols"   // 巡检记录
	TypeHandover  = "handovers" // 交接班记录
	TypeOperation = "opslogs"   // 设备操作记录
)

// defaultDefs 是内置类型的唯一定义表，类型名与取数方式集中在此维护。
var defaultDefs = []struct {
	key, name string
}{
	{TypeAlert, "告警记录"},
	{TypePatrol, "巡检记录"},
	{TypeHandover, "交接班记录"},
	{TypeOperation, "设备操作记录"},
}

// RegisterDefault 把内置资料类型注册到 exporter。
func RegisterDefault(exporter *Exporter, store *ItemStore) error {
	for _, def := range defaultDefs {
		mat := TextMaterial{
			TypeKey:  def.key,
			TypeName: def.name,
			Query:    store.Query(def.key),
		}
		if err := exporter.Register(mat); err != nil {
			return err
		}
	}
	return nil
}
