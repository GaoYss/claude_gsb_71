package repair_test

import (
	"context"
	"net/http"
	"testing"

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

// createFaultReportedAt 登记一条指定上报时间的故障, 便于构造补录场景。
func (h *harness) createFaultReportedAt(t *testing.T, lampID uint, reportedAt string) *fault.Fault {
	t.Helper()
	entity, err := h.faults.Create(context.Background(), fault.CreateRequest{
		LampID:      lampID,
		FaultType:   "灯不亮",
		FaultLevel:  fault.LevelHigh,
		Source:      fault.SourceInspection,
		Description: "补录口径测试",
		Reporter:    "巡检员",
		ReportedAt:  reportedAt,
	})
	require.NoError(t, err)
	return entity
}

func TestBackfilledEarlierRepairKeepsOccurrenceConclusion(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-101")
	entity := h.createFaultReportedAt(t, device.ID, "2026-09-20 08:00:00")

	// 先发生并登记的维修: 10:00 开工, 12:00 完工, 结果待配件, 故障保持维修中
	first, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工甲", StartedAt: "2026-09-20 10:00:00",
	})
	require.NoError(t, err)
	_, err = h.repairs.Finish(ctx, first.ID, repair.FinishRequest{
		Result: repair.ResultPendingParts, FinishedAt: "2026-09-20 12:00:00",
	})
	require.NoError(t, err)

	// 补录发生时间更早的维修: 09:00 开工
	backfilled, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工乙", StartedAt: "2026-09-20 09:00:00",
	})
	require.NoError(t, err)

	afterBackfill, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 2, afterBackfill.RepairCount)
	require.NotNil(t, afterBackfill.LatestRepairID)
	require.Equal(t, first.ID, *afterBackfill.LatestRepairID,
		"最近一次维修应按发生时间认定, 不随补录登记顺序漂移")

	// 补录记录完工, 结果已修复: 不允许推翻更晚发生维修(待配件)形成的处置结论
	_, err = h.repairs.Finish(ctx, backfilled.ID, repair.FinishRequest{
		Result: repair.ResultFixed, FinishedAt: "2026-09-20 09:30:00",
	})
	require.NoError(t, err)

	afterFinish, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusProcessing, afterFinish.Status,
		"补录的更早记录完工不能改变故障当前处置结论")
	require.Equal(t, first.ID, *afterFinish.LatestRepairID)

	deviceAfter, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, deviceAfter.RunStatus,
		"故障仍在维修中, 路灯运行状态不应被补录记录带偏")
}

func TestBackfilledLaterRepairDrivesConclusion(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-102")
	entity := h.createFaultReportedAt(t, device.ID, "2026-09-20 08:00:00")

	// 先登记 09:00 开工的维修, 完工待配件
	first, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工甲", StartedAt: "2026-09-20 09:00:00",
	})
	require.NoError(t, err)
	_, err = h.repairs.Finish(ctx, first.ID, repair.FinishRequest{
		Result: repair.ResultPendingParts, FinishedAt: "2026-09-20 10:00:00",
	})
	require.NoError(t, err)

	// 补录发生时间更晚的维修: 11:00 开工, 12:00 完工, 结果已修复
	later, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工乙", StartedAt: "2026-09-20 11:00:00",
	})
	require.NoError(t, err)
	_, err = h.repairs.Finish(ctx, later.ID, repair.FinishRequest{
		Result: repair.ResultFixed, FinishedAt: "2026-09-20 12:00:00",
	})
	require.NoError(t, err)

	afterFinish, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusRepaired, afterFinish.Status,
		"发生时间更晚的维修完工结果应正常驱动故障结论")
	require.Equal(t, 2, afterFinish.RepairCount)
	require.Equal(t, later.ID, *afterFinish.LatestRepairID)
}

func TestDeleteRepairRecalculatesLatestByOccurrence(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-103")
	entity := h.createFaultReportedAt(t, device.ID, "2026-09-20 08:00:00")

	first, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工甲", StartedAt: "2026-09-20 10:00:00",
	})
	require.NoError(t, err)
	_, err = h.repairs.Finish(ctx, first.ID, repair.FinishRequest{
		Result: repair.ResultPendingParts, FinishedAt: "2026-09-20 12:00:00",
	})
	require.NoError(t, err)

	backfilled, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工乙", StartedAt: "2026-09-20 09:00:00",
	})
	require.NoError(t, err)
	_, err = h.repairs.Finish(ctx, backfilled.ID, repair.FinishRequest{
		Result: repair.ResultFixed, FinishedAt: "2026-09-20 09:30:00",
	})
	require.NoError(t, err)

	// 删除发生时间最晚的记录后, 最近一次维修重算为补录的那条
	require.NoError(t, h.repairs.Delete(ctx, first.ID))
	afterDelete, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 1, afterDelete.RepairCount)
	require.NotNil(t, afterDelete.LatestRepairID)
	require.Equal(t, backfilled.ID, *afterDelete.LatestRepairID)

	// 全部删除后次数归零, 故障回退待处理
	require.NoError(t, h.repairs.Delete(ctx, backfilled.ID))
	afterClear, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 0, afterClear.RepairCount)
	require.Nil(t, afterClear.LatestRepairID)
	require.Equal(t, fault.StatusPending, afterClear.Status)
}

func TestUpdateStartedAtRecalculatesLatest(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-104")
	entity := h.createFaultReportedAt(t, device.ID, "2026-09-20 08:00:00")

	first, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工甲", StartedAt: "2026-09-20 10:00:00",
	})
	require.NoError(t, err)
	_, err = h.repairs.Finish(ctx, first.ID, repair.FinishRequest{
		Result: repair.ResultPendingParts, FinishedAt: "2026-09-20 12:00:00",
	})
	require.NoError(t, err)

	second, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工乙", StartedAt: "2026-09-20 13:00:00",
	})
	require.NoError(t, err)

	afterSecond, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, second.ID, *afterSecond.LatestRepairID)

	// 把进行中记录的开工时间改早, 最近一次维修应重算回原来那条
	earlier := "2026-09-20 09:00:00"
	_, err = h.repairs.Update(ctx, second.ID, repair.UpdateRequest{StartedAt: &earlier})
	require.NoError(t, err)

	afterUpdate, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, first.ID, *afterUpdate.LatestRepairID,
		"开工时间被改早后, 最近一次维修应按发生时间重算")
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
