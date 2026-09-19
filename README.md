# solo-6600009 - IoT Geofence Monitor

## Tech
- **Frontend**: Vue 3 + Pinia + Leaflet
- **Backend**: Go + Gin + MQTT
- **Database**: TimescaleDB

## Start
```bash
cd backend && go run main.go
cd frontend && npm install && npm run dev
```

## 值班资料导出（backend/internal/dutyexport）

筛选、文件组装、归档摘要、恢复判断共用同一条管线与同一类型注册表，
避免多处维护导致摘要与归档内容不一致：

```
MaterialType 注册（唯一口径）
        │
   buildOne：时间窗筛选 [00:00, 次日00:00) → 稳定排序 → 组装文件 → 摘要推导
        │
 ┌──────┼─────────┬──────────┐
导出   归档       恢复        历史回看
```

- 已归档记录不可重写：再次归档原样保留（`AlreadyArchived`），时钟推进也不改 `ArchivedAt`。
- 部分资料缺失：导出/摘要照常标注"资料缺失"，归档跳过该类型，资料补齐后再归档才写入。
- 重复恢复：有归档永远返回归档原件（逐字节一致），无归档时实时组装且不落归档。
- 历史回看：只读归档，不受数据源后续变化影响。

### HTTP 接口
| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/duty/materials` | 已注册资料类型清单 |
| GET | `/api/duty/export?date=2026-09-19&types=a,b` | 按值班日导出（types 可省略） |
| POST | `/api/duty/archive` | body `{"date":"2026-09-19"}`，按日归档 |
| GET | `/api/duty/restore?date=...&type=...` | 恢复，优先归档原件 |
| GET | `/api/duty/history?date=...` | 历史归档回看 |

### 新增一种资料类型（只改一处）
实现/注册一个 `MaterialType` 即可，筛选、组装、归档、恢复、历史全部自动生效：

```go
exporter.Register(dutyexport.TextMaterial{
    TypeKey:  "weather",
    TypeName: "气象记录",
    Query:    store.Query("weather"), // 或任何 func(from, to time.Time) ([]Item, error)
})
```

时间窗的精确过滤与排序由管线统一兜底，类型实现即使越界取数也不会污染结果。

### 测试
```bash
cd backend && go test ./internal/dutyexport/
```
