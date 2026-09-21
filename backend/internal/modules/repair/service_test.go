package repair_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
)

// harness 使用内存数据库装配真实模块, 用于验证跨模块业务流程。
type harness struct {
	lamps   *lamp.Service
	faults  *fault.Service
	repairs *repair.Service
	db      *gorm.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	require.NoError(t, err)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	require.NoError(t, db.AutoMigrate(&lamp.Lamp{}, &fault.Fault{}, &repair.Repair{}))

	lampRepository := lamp.NewRepository(db)
	lampService := lamp.NewService(lampRepository)

	faultRepository := fault.NewRepository(db)
	faultService := fault.NewService(faultRepository, lampService)
	lampService.SetOpenFaultCounter(faultRepository)

	repairRepository := repair.NewRepository(db)
	repairService := repair.NewService(repairRepository, faultService)

	return &harness{lamps: lampService, faults: faultService, repairs: repairService, db: db}
}

func (h *harness) createLamp(t *testing.T, code string) *lamp.Lamp {
	t.Helper()
	entity, err := h.lamps.Create(context.Background(), lamp.CreateRequest{
		Code:     code,
		Name:     "测试灯杆",
		RoadName: "测试路",
		LampType: lamp.LampTypeLED,
	})
	require.NoError(t, err)
	return entity
}

func (h *harness) createFault(t *testing.T, lampID uint, description string) *fault.Fault {
	t.Helper()
	entity, err := h.faults.Create(context.Background(), fault.CreateRequest{
		LampID:      lampID,
		FaultType:   "灯不亮",
		FaultLevel:  fault.LevelHigh,
		Source:      fault.SourceInspection,
		Description: description,
		Reporter:    "巡检员",
	})
	require.NoError(t, err)
	return entity
}

// requireConflict 断言错误是 409 业务冲突。
func requireConflict(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	businessErr, ok := apperr.As(err)
	require.True(t, ok, "期望业务错误, 实际: %v", err)
	require.Equal(t, http.StatusConflict, businessErr.Status, "错误信息: %s", businessErr.Message)
}

func TestFaultRepairLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-001")

	entity := h.createFault(t, device.ID, "整灯不亮, 疑似驱动电源故障")
	require.Equal(t, fault.StatusPending, entity.Status)
	require.Regexp(t, `^GD\d{8}\d{4}$`, entity.FaultNo)

	afterReport, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusFault, afterReport.RunStatus, "登记故障后路灯应变为故障状态")

	// 同一盏路灯不允许存在多条未闭环故障
	_, err = h.faults.Create(ctx, fault.CreateRequest{
		LampID: device.ID, FaultType: "灯不亮", Description: "重复登记",
	})
	requireConflict(t, err)

	// 维修开工
	record, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工甲", RepairTeam: "市政照明一班",
	})
	require.NoError(t, err)
	require.Equal(t, repair.StatusOngoing, record.Status)
	require.Regexp(t, `^WX\d{8}\d{4}$`, record.RepairNo)

	faultAfterStart, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusProcessing, faultAfterStart.Status)
	require.Equal(t, 1, faultAfterStart.RepairCount)
	require.NotNil(t, faultAfterStart.LatestRepairID)
	require.Equal(t, record.ID, *faultAfterStart.LatestRepairID)

	lampAfterStart, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, lampAfterStart.RunStatus)

	// 同一故障不允许并行开工
	_, err = h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工乙"})
	requireConflict(t, err)

	// 完工且结果为已修复
	cost := 210.0
	finished, err := h.repairs.Finish(ctx, record.ID, repair.FinishRequest{
		Result: repair.ResultFixed, Content: "更换驱动电源", Cost: &cost,
	})
	require.NoError(t, err)
	require.Equal(t, repair.StatusFinished, finished.Status)
	require.NotNil(t, finished.FinishedAt)

	faultAfterFinish, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusRepaired, faultAfterFinish.Status)

	lampAfterFinish, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusNormal, lampAfterFinish.RunStatus, "修复后路灯应恢复为正常")

	// 关闭故障形成闭环
	closed, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "现场复核通过"})
	require.NoError(t, err)
	require.Equal(t, fault.StatusClosed, closed.Status)
	require.NotNil(t, closed.ClosedAt)

	// 已产生的维修记录使故障不可删除
	requireConflict(t, h.faults.Delete(ctx, entity.ID))
	// 已关闭故障不允许再次关闭
	_, err = h.faults.Close(ctx, entity.ID, fault.CloseRequest{})
	requireConflict(t, err)
}

func TestRepairPendingPartsKeepsFaultProcessing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-002")
	entity := h.createFault(t, device.ID, "线路老化需要更换电缆")

	record, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工丙",
	})
	require.NoError(t, err)

	_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultPendingParts})
	require.NoError(t, err)

	faultAfterFinish, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusProcessing, faultAfterFinish.Status, "非已修复结果不应结束故障")

	lampAfterFinish, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, lampAfterFinish.RunStatus)

	// 可继续登记第二次维修(返修)
	second, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工丙", Content: "物料到场后更换电缆",
	})
	require.NoError(t, err)
	require.Equal(t, repair.StatusOngoing, second.Status)

	faultAfterSecond, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 2, faultAfterSecond.RepairCount)
}

func TestRepairBackfillKeepsLatestOccurrenceAndConclusion(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-005")
	entity := h.createFault(t, device.ID, "补录历史维修记录的口径校验")

	reported := entity.ReportedAt
	layout := "2006-01-02 15:04:05"
	startA := reported.Add(20 * time.Hour).Format(layout)
	finishA := reported.Add(21 * time.Hour).Format(layout)
	startB := reported.Add(2 * time.Hour).Format(layout)
	finishB := reported.Add(3 * time.Hour).Format(layout)

	// 最近一次(发生时间)维修: 已修复, 故障形成"已修复"结论。
	recordA, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工甲", StartedAt: startA,
	})
	require.NoError(t, err)
	_, err = h.repairs.Finish(ctx, recordA.ID, repair.FinishRequest{
		Result: repair.ResultFixed, FinishedAt: finishA,
	})
	require.NoError(t, err)

	faultAfterA, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusRepaired, faultAfterA.Status)
	require.Equal(t, 1, faultAfterA.RepairCount)
	require.NotNil(t, faultAfterA.LatestRepairID)
	require.Equal(t, recordA.ID, *faultAfterA.LatestRepairID)

	// 补录一条发生时间更早、结果为待配件的历史维修。
	recordB, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工乙", StartedAt: startB,
	})
	require.NoError(t, err, "已修复但未关闭的故障应允许补录维修记录")
	_, err = h.repairs.Finish(ctx, recordB.ID, repair.FinishRequest{
		Result: repair.ResultPendingParts, FinishedAt: finishB,
	})
	require.NoError(t, err)

	faultAfterBackfill, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 2, faultAfterBackfill.RepairCount, "补录后维修次数应累加")
	require.NotNil(t, faultAfterBackfill.LatestRepairID)
	require.Equal(t, recordA.ID, *faultAfterBackfill.LatestRepairID,
		"最近一次维修应仍按发生时间取 recordA, 不能被后登记的 recordB 顶替")
	require.Equal(t, fault.StatusRepaired, faultAfterBackfill.Status,
		"补录更早的非已修复记录不能推翻已经形成的已修复结论")

	// 处置时间线/维修过程按发生时间排序: B 在前, A 在后。
	repairs, err := h.repairs.ListByFault(ctx, entity.ID)
	require.NoError(t, err)
	require.Len(t, repairs, 2)
	require.Equal(t, recordB.ID, repairs[0].ID)
	require.Equal(t, recordA.ID, repairs[1].ID)

	// 删除补录的记录后, 结论与最近一次维修仍保持在 recordA 上。
	require.NoError(t, h.repairs.Delete(ctx, recordB.ID))
	faultAfterDeleteBackfill, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 1, faultAfterDeleteBackfill.RepairCount)
	require.Equal(t, recordA.ID, *faultAfterDeleteBackfill.LatestRepairID)
	require.Equal(t, fault.StatusRepaired, faultAfterDeleteBackfill.Status)

	// 删除真正的最近一次维修后, 故障状态按剩余记录(recordB 已删, 无记录)回到待处理。
	require.NoError(t, h.repairs.Delete(ctx, recordA.ID))
	faultAfterDeleteLatest, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 0, faultAfterDeleteLatest.RepairCount)
	require.Nil(t, faultAfterDeleteLatest.LatestRepairID)
	require.Equal(t, fault.StatusPending, faultAfterDeleteLatest.Status)
}

func TestRepairRejectedOnClosedFault(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-003")
	entity := h.createFault(t, device.ID, "误报故障需要作废")

	_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "误报作废"})
	require.NoError(t, err)

	_, err = h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工丁"})
	requireConflict(t, err)
}

func TestFaultValidation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-004")

	// 非法故障类型
	_, err := h.faults.Create(ctx, fault.CreateRequest{
		LampID: device.ID, FaultType: "不存在的类型", Description: "测试",
	})
	businessErr, ok := apperr.As(err)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, businessErr.Status)

	// 路灯不存在
	_, err = h.faults.Create(ctx, fault.CreateRequest{
		LampID: 99999, FaultType: "灯不亮", Description: "测试",
	})
	businessErr, ok = apperr.As(err)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, businessErr.Status)

	// 重复路灯编号
	_, err = h.lamps.Create(ctx, lamp.CreateRequest{
		Code: device.Code, RoadName: "测试路", LampType: lamp.LampTypeLED,
	})
	requireConflict(t, err)

	// 存在未闭环故障时不允许删除路灯
	h.createFault(t, device.ID, "删除校验")
	requireConflict(t, h.lamps.Delete(ctx, device.ID))
}
