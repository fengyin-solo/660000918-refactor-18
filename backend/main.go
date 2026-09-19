package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yuenlai/solo-6600009/internal/dutyexport"
)

type Device struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`
	Status      string  `json:"status"`
	Battery     int     `json:"battery"`
	Temperature float64 `json:"temperature"`
}

var devices = []Device{
	{ID: "d1", Name: "Sensor-A01", Lat: 39.9042, Lng: 116.4074, Status: "online", Battery: 85, Temperature: 24.5},
	{ID: "d2", Name: "Sensor-B02", Lat: 39.9142, Lng: 116.3974, Status: "alert", Battery: 12, Temperature: 38.2},
}

// newDutyExporter 组装值班资料导出的统一入口：
// 数据源、资料类型注册表、归档存储各只有一份，所有 HTTP 处理共用。
func newDutyExporter() *dutyexport.Exporter {
	loc := time.FixedZone("CST", 8*60*60)
	day := "2026-09-19"
	store := dutyexport.NewItemStore(
		dutyexport.Item{Type: dutyexport.TypeAlert, ID: "AL-1001", Timestamp: mustTime(day, "02:14"), Title: "Sensor-B02 电量过低"},
		dutyexport.Item{Type: dutyexport.TypeAlert, ID: "AL-1002", Timestamp: mustTime(day, "08:47"), Title: "Sensor-B02 进入危险区域"},
		dutyexport.Item{Type: dutyexport.TypePatrol, ID: "PA-2001", Timestamp: mustTime(day, "09:30"), Title: "生产车间例行巡检，正常"},
		dutyexport.Item{Type: dutyexport.TypeHandover, ID: "HO-3001", Timestamp: mustTime(day, "20:00"), Title: "夜班交接：2 条未确认告警"},
		// TypeOperation 当天故意无记录，用于演示"部分资料缺失"路径。
	)
	exporter := dutyexport.New(loc)
	if err := dutyexport.RegisterDefault(exporter, store); err != nil {
		panic(err)
	}
	return exporter
}

func mustTime(date, hhmm string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+hhmm, time.FixedZone("CST", 8*60*60))
	if err != nil {
		panic(err)
	}
	return t
}

func registerDutyRoutes(r *gin.Engine, exporter *dutyexport.Exporter) {
	g := r.Group("/api/duty")

	// 资料类型清单：新增类型后此处自动反映，无需改动路由代码。
	g.GET("/materials", func(c *gin.Context) {
		specs := exporter.RegisteredTypes()
		out := make([]gin.H, 0, len(specs))
		for _, s := range specs {
			out = append(out, gin.H{"type": s.Key(), "name": s.DisplayName()})
		}
		c.JSON(http.StatusOK, out)
	})

	// 按值班日导出（?date=YYYY-MM-DD&types=a,b，types 可空）。
	g.GET("/export", func(c *gin.Context) {
		date := c.Query("date")
		var keys []string
		if raw := c.Query("types"); raw != "" {
			for _, k := range splitCSV(raw) {
				keys = append(keys, k)
			}
		}
		bundle, err := exporter.ExportBundle(date, keys)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, bundle)
	})

	// 按值班日归档；已归档类型原样保留，缺失类型跳过。
	g.POST("/archive", func(c *gin.Context) {
		var req struct {
			Date string `json:"date"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		outcomes, err := exporter.Archive(req.Date)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, outcomes)
	})

	// 恢复某类资料：优先归档原件，否则实时组装。
	g.GET("/restore", func(c *gin.Context) {
		view, archived, err := exporter.Restore(c.Query("date"), c.Query("type"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"archived": archived, "material": view})
	})

	// 某值班日的归档历史回看。
	g.GET("/history", func(c *gin.Context) {
		records, err := exporter.History(c.Query("date"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, records)
	})
}

func splitCSV(raw string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i == len(raw) || raw[i] == ',' {
			if part := raw[start:i]; part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func main() {
	r := gin.Default()
	r.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "IoT Geofence Monitor"})
	})
	r.GET("/api/devices", func(c *gin.Context) {
		c.JSON(http.StatusOK, devices)
	})
	r.POST("/api/devices", func(c *gin.Context) {
		var d Device
		if err := c.BindJSON(&d); err != nil {
			c.JSON(400, err)
			return
		}
		devices = append(devices, d)
		c.JSON(http.StatusCreated, d)
	})

	registerDutyRoutes(r, newDutyExporter())

	fmt.Println("Server on :8080")
	r.Run(":8080")
}
