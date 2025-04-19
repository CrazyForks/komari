package clients

import (
	"encoding/json"
	"fmt"

	"github.com/akizon77/komari/database/dbcore"
	"github.com/akizon77/komari/database/models"
	"github.com/akizon77/komari_common"

	"gorm.io/gorm"
)

// Report 表示客户端报告数据
// SaveReport 保存报告数据
func SaveReport(data map[string]interface{}) (err error) {
	token := data["token"].(string)
	clientUUID, err := GetClientUUIDByToken(token)
	if err != nil {
		return err
	}
	report, err := ParseReport(data)
	if err != nil {
		return err
	}
	err = SaveClientReport(clientUUID, report)
	if err != nil {

		return err
	}
	return nil

}

func GetClientUUIDByToken(token string) (clientUUID string, err error) {
	db := dbcore.GetDBInstance()
	err = db.Model(&models.Client{}).Where("token = ?", token).First(&clientUUID).Error
	if err != nil {
		return "", err
	}
	return clientUUID, nil
}

func ParseReport(data map[string]interface{}) (report komari_common.Report, err error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return komari_common.Report{}, err
	}
	err = json.Unmarshal(jsonData, &report)
	if err != nil {
		return komari_common.Report{}, err
	}
	return report, nil
}

// SaveClientReport 保存客户端报告到 History 表
func SaveClientReport(clientUUID string, report komari_common.Report) (err error) {
	db := dbcore.GetDBInstance()

	history := models.History{
		CPU:            float32(report.CPU.Usage),
		GPU:            0, // Report 未提供 GPU Usage，设为 0（与原 nil 行为类似）
		RAM:            report.Ram.Used,
		RAMTotal:       report.Ram.Total,
		SWAP:           report.Swap.Used,
		SWAPTotal:      report.Swap.Total,
		LOAD:           float32(report.Load.Load1), // 使用 Load1 作为主要负载指标
		TEMP:           0,                          // Report 未提供 TEMP，设为 0（与原 nil 行为类似）
		DISK:           report.Disk.Used,
		DISKTotal:      report.Disk.Total,
		NETIn:          report.Network.Down,
		NETOut:         report.Network.Up,
		NETTotalUp:     report.Network.TotalUp,
		NETTotalDown:   report.Network.TotalDown,
		PROCESS:        report.Process,
		Connections:    report.Connections.TCP,
		ConnectionsUDP: report.Connections.UDP,
	}

	// 使用事务确保 History 和 ClientsInfo 一致性
	err = db.Transaction(func(tx *gorm.DB) error {
		// 保存 History
		if err := tx.Create(&history).Error; err != nil {
			return fmt.Errorf("failed to save history: %v", err)
		}
		return nil
	})

	if err != nil {
		return err
	}

	return nil
}

// getString 从 map 中获取字符串
func getString(data map[string]interface{}, key string) string {
	if val, ok := data[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}

// getInt 从 map 中获取整数
func getInt(data map[string]interface{}, key string) int {
	if val, ok := data[key]; ok {
		if num, ok := val.(float64); ok {
			return int(num)
		}
	}
	return 0
}

// getInt64 从 map 中获取 int64
func getInt64(data map[string]interface{}, key string) int64 {
	if val, ok := data[key]; ok {
		if num, ok := val.(float64); ok {
			return int64(num)
		}
	}
	return 0
}

// getFloat 从 map 中获取 float64
func getFloat(data map[string]interface{}, key string) float64 {
	if val, ok := data[key]; ok {
		if num, ok := val.(float64); ok {
			return num
		}
	}
	return 0.0
}
