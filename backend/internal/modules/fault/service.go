package fault

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/lamp"
	"streetlight/pkg/pagination"
)

// faultSortSpec 定义故障列表接口允许的排序字段白名单。
var faultSortSpec = pagination.SortSpec{
	Allowed: map[string]string{
		"fault_no":    "fault_no",
		"reported_at": "reported_at",
		"status":      "status",
		"fault_type":  "fault_type",
		"fault_level": "fault_level",
		"created_at":  "created_at",
		"updated_at":  "updated_at",
	},
	Default: "reported_at",
}

// LampPort 由路灯台账模块实现, 故障模块通过它读取路灯信息并联动运行状态。
type LampPort interface {
	Get(ctx context.Context, id uint) (*lamp.Lamp, error)
	UpdateRunStatus(ctx context.Context, id uint, status string) error
}

// Service 承载故障登记的业务规则, 并向维修模块提供故障状态流转能力。
type Service struct {
	repo  *Repository
	lamps LampPort
}

// NewService 构造故障登记服务。
func NewService(repo *Repository, lamps LampPort) *Service {
	return &Service{repo: repo, lamps: lamps}
}

// Repository 暴露仓储, 供 bootstrap 装配其它模块所需的端口。
func (s *Service) Repository() *Repository { return s.repo }

// GetByID 查询故障详情, 同时满足维修模块 FaultPort 端口定义。
func (s *Service) GetByID(ctx context.Context, id uint) (*Fault, error) {
	return s.repo.GetByID(ctx, id)
}

// GetByNo 按故障单号查询故障。
func (s *Service) GetByNo(ctx context.Context, faultNo string) (*Fault, error) {
	return s.repo.GetByNo(ctx, faultNo)
}

// List 分页查询故障列表, 同时返回归一化后的分页信息。
func (s *Service) List(ctx context.Context, query ListQuery) ([]Fault, int64, pagination.Query, error) {
	page := pagination.Parse(query.Params, faultSortSpec)
	filter, err := buildFilter(query)
	if err != nil {
		return nil, 0, page, err
	}
	items, total, err := s.repo.List(ctx, filter, page)
	if err != nil {
		return nil, 0, page, err
	}
	return items, total, page, nil
}

// ListByLamp 查询某盏路灯的故障历史。
func (s *Service) ListByLamp(ctx context.Context, lampID uint) ([]Fault, error) {
	return s.repo.ListByLamp(ctx, lampID)
}

// Create 登记故障: 校验路灯存在、无未闭环故障后落库, 并同步路灯运行状态。
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Fault, error) {
	device, err := s.lamps.Get(ctx, req.LampID)
	if err != nil {
		return nil, err
	}

	openCount, err := s.repo.CountOpenByLamp(ctx, device.ID)
	if err != nil {
		return nil, err
	}
	if openCount > 0 {
		return nil, apperr.Conflict("路灯 %s 已存在 %d 条未闭环故障, 请先处理后再登记", device.Code, openCount)
	}

	faultType := strings.TrimSpace(req.FaultType)
	if !isValidFaultType(faultType) {
		return nil, apperr.BadRequest("非法的故障类型: %s", faultType)
	}

	description := strings.TrimSpace(req.Description)
	if description == "" {
		return nil, apperr.BadRequest("故障描述不能为空")
	}

	level := strings.TrimSpace(req.FaultLevel)
	if level == "" {
		level = LevelNormal
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = SourceInspection
	}

	reportedAt, err := parseReportedAt(req.ReportedAt)
	if err != nil {
		return nil, err
	}

	entity := &Fault{
		LampID:        device.ID,
		LampCode:      device.Code,
		RoadName:      device.RoadName,
		FaultType:     faultType,
		FaultLevel:    level,
		Source:        source,
		Description:   description,
		Reporter:      strings.TrimSpace(req.Reporter),
		ReporterPhone: strings.TrimSpace(req.ReporterPhone),
		ReportedAt:    reportedAt,
		Status:        StatusPending,
	}

	if err := s.repo.CreateWithUniqueNo(ctx, entity, faultNoPrefix(reportedAt)); err != nil {
		return nil, err
	}

	if err := s.syncLampStatus(ctx, device.ID); err != nil {
		slog.Warn("同步路灯运行状态失败", "lamp_id", device.ID, "fault_no", entity.FaultNo, "error", err)
	}
	return entity, nil
}

// Update 修改故障登记信息, 已关闭的故障不允许修改。
func (s *Service) Update(ctx context.Context, id uint, req UpdateRequest) (*Fault, error) {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity.Status == StatusClosed {
		return nil, apperr.Conflict("故障 %s 已关闭, 不允许修改", entity.FaultNo)
	}

	if req.FaultType != nil {
		value := strings.TrimSpace(*req.FaultType)
		if !isValidFaultType(value) {
			return nil, apperr.BadRequest("非法的故障类型: %s", value)
		}
		entity.FaultType = value
	}
	if req.FaultLevel != nil {
		entity.FaultLevel = strings.TrimSpace(*req.FaultLevel)
	}
	if req.Source != nil {
		entity.Source = strings.TrimSpace(*req.Source)
	}
	if req.Description != nil {
		value := strings.TrimSpace(*req.Description)
		if value == "" {
			return nil, apperr.BadRequest("故障描述不能为空")
		}
		entity.Description = value
	}
	if req.Reporter != nil {
		entity.Reporter = strings.TrimSpace(*req.Reporter)
	}
	if req.ReporterPhone != nil {
		entity.ReporterPhone = strings.TrimSpace(*req.ReporterPhone)
	}
	if req.ReportedAt != nil {
		value, err := parseReportedAt(*req.ReportedAt)
		if err != nil {
			return nil, err
		}
		entity.ReportedAt = value
	}

	if err := s.repo.Update(ctx, entity); err != nil {
		return nil, err
	}
	return entity, nil
}

// Close 关闭故障, 用于确认闭环或作废处理。
func (s *Service) Close(ctx context.Context, id uint, req CloseRequest) (*Fault, error) {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity.Status == StatusClosed {
		return nil, apperr.Conflict("故障 %s 已关闭, 无需重复操作", entity.FaultNo)
	}
	if !canTransitTo(entity.Status, StatusClosed) {
		return nil, apperr.Conflict("故障 %s 当前状态为 %s, 不允许关闭", entity.FaultNo, StatusLabel(entity.Status))
	}

	now := time.Now()
	entity.Status = StatusClosed
	entity.ClosedAt = &now
	entity.CloseRemark = strings.TrimSpace(req.Remark)

	if err := s.repo.Update(ctx, entity); err != nil {
		return nil, err
	}
	if err := s.syncLampStatus(ctx, entity.LampID); err != nil {
		slog.Warn("同步路灯运行状态失败", "lamp_id", entity.LampID, "fault_no", entity.FaultNo, "error", err)
	}
	return entity, nil
}

// Delete 删除故障, 仅允许删除已关闭且没有维修记录的故障。
func (s *Service) Delete(ctx context.Context, id uint) error {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if entity.Status != StatusClosed {
		return apperr.Conflict("仅已关闭的故障允许删除, 当前状态: %s", StatusLabel(entity.Status))
	}
	if entity.RepairCount > 0 {
		return apperr.Conflict("该故障已产生 %d 条维修记录, 不允许删除", entity.RepairCount)
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	if err := s.syncLampStatus(ctx, entity.LampID); err != nil {
		slog.Warn("同步路灯运行状态失败", "lamp_id", entity.LampID, "error", err)
	}
	return nil
}

// Metadata 返回故障模块字典。
func (s *Service) Metadata() *Meta {
	return &Meta{
		Statuses:   Statuses(),
		Levels:     Levels(),
		Sources:    Sources(),
		FaultTypes: FaultTypes(),
	}
}

// RepairSnapshot 是维修侧在维修记录发生变化后提供的故障维修现状快照。
// "最近一次维修"统一按发生时间(开工时间 started_at DESC, id DESC)判定, 与处置时间线、
// 维修记录列表、维修状态查询的口径保持一致, 不以登记先后(created_at)为准。
type RepairSnapshot struct {
	Count         int   // 该故障的维修记录总数
	LatestID      *uint // 按发生时间最近的一次维修记录 ID, 无记录时为 nil
	LatestOngoing bool  // 最近一次维修仍在进行中
	LatestFixed   bool  // 最近一次维修已完工且处置结果为"已修复"
}

// SyncAfterRepairChange 在维修记录录入/完工/修改/删除后对账故障数据:
// 重算维修次数与最近一次维修, 并按最近一次维修的结论对账故障状态与路灯状态。
//
// 关键约束: 补录发生时间更早的历史维修时, 最近一次维修不变, 故障状态保持原结论;
// 已关闭的故障视为终态, 只回写统计数据, 状态不再被维修侧改动。
func (s *Service) SyncAfterRepairChange(ctx context.Context, faultID uint, snap RepairSnapshot) error {
	entity, err := s.repo.GetByID(ctx, faultID)
	if err != nil {
		return err
	}

	columns := map[string]any{
		"repair_count":     snap.Count,
		"latest_repair_id": snap.LatestID,
	}
	if entity.Status != StatusClosed {
		columns["status"] = resolveStatusAfterRepair(snap)
	}

	if err := s.repo.UpdateColumns(ctx, faultID, columns); err != nil {
		return err
	}
	return s.syncLampStatus(ctx, entity.LampID)
}

// resolveStatusAfterRepair 依据最近一次维修(发生时间口径)的结论推算故障状态:
// 无记录回到待处理, 最近一次在修为维修中, 最近一次完工已修复为已修复,
// 最近一次完工但未修复(待配件/观察中/无法修复)保持维修中等待后续处置。
func resolveStatusAfterRepair(snap RepairSnapshot) string {
	switch {
	case snap.Count == 0 || snap.LatestID == nil:
		return StatusPending
	case snap.LatestOngoing:
		return StatusProcessing
	case snap.LatestFixed:
		return StatusRepaired
	default:
		return StatusProcessing
	}
}

// syncLampStatus 依据该路灯的故障分布重新计算并写回运行状态。
func (s *Service) syncLampStatus(ctx context.Context, lampID uint) error {
	counts, err := s.repo.StatusCountsForLamp(ctx, lampID)
	if err != nil {
		return err
	}

	status := lamp.RunStatusNormal
	switch {
	case counts[StatusProcessing] > 0:
		status = lamp.RunStatusMaintenance
	case counts[StatusPending] > 0:
		status = lamp.RunStatusFault
	}
	return s.lamps.UpdateRunStatus(ctx, lampID, status)
}

// buildFilter 将列表查询参数转换为仓储条件, 并解析日期区间。
func buildFilter(query ListQuery) (Filter, error) {
	filter := Filter{
		Keyword:    strings.TrimSpace(query.Keyword),
		Status:     strings.TrimSpace(query.Status),
		FaultType:  strings.TrimSpace(query.FaultType),
		FaultLevel: strings.TrimSpace(query.FaultLevel),
		Source:     strings.TrimSpace(query.Source),
		LampID:     query.LampID,
		RoadName:   strings.TrimSpace(query.RoadName),
		OnlyOpen:   query.OnlyOpen,
		NotClosed:  query.NotClosed,
	}

	if filter.Status != "" && !IsValidStatus(filter.Status) {
		return filter, apperr.BadRequest("非法的故障状态: %s", filter.Status)
	}

	if strings.TrimSpace(query.StartDate) != "" {
		from, err := parseDay(query.StartDate)
		if err != nil {
			return filter, err
		}
		filter.ReportedFrom = &from
	}
	if strings.TrimSpace(query.EndDate) != "" {
		to, err := parseDay(query.EndDate)
		if err != nil {
			return filter, err
		}
		to = to.AddDate(0, 0, 1)
		filter.ReportedTo = &to
	}
	if filter.ReportedFrom != nil && filter.ReportedTo != nil && filter.ReportedTo.Before(*filter.ReportedFrom) {
		return filter, apperr.BadRequest("结束日期不能早于开始日期")
	}
	return filter, nil
}

// faultNoPrefix 生成故障单号前缀, 例如 GD20260913。
func faultNoPrefix(reportedAt time.Time) string {
	return "GD" + reportedAt.Format("20060102")
}

// parseReportedAt 解析上报时间, 支持 RFC3339 与常见的日期时间格式, 为空时取当前时间。
func parseReportedAt(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Now(), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, apperr.BadRequest("上报时间格式不正确, 建议使用 YYYY-MM-DD HH:mm:ss: %s", value)
}

// parseDay 解析 YYYY-MM-DD 日期, 返回当天零点。
func parseDay(value string) (time.Time, error) {
	date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(value), time.Local)
	if err != nil {
		return time.Time{}, apperr.BadRequest("日期格式应为 YYYY-MM-DD, 当前值: %s", value)
	}
	return date, nil
}
