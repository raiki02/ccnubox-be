package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/asynccnu/ccnubox-be/be-classlist/internal/conf"
	"github.com/asynccnu/ccnubox-be/be-classlist/internal/errcode"
	"github.com/asynccnu/ccnubox-be/be-classlist/internal/model"
	"github.com/asynccnu/ccnubox-be/be-classlist/internal/pkg/tool"
	"github.com/go-kratos/kratos/v2/log"
	"sync"
	"time"
)

type ClassUsecase struct {
	classRepo ClassRepoProxy
	crawler   ClassCrawler
	ccnu      CCNUServiceProxy
	jxbRepo   JxbRepo
	delayQue  DelayQueue

	refreshLogRepo  RefreshLogRepo
	waitCrawTime    time.Duration
	waitUserSvcTime time.Duration
	log             *log.Helper
}

func NewClassUsecase(classRepo ClassRepoProxy, crawler ClassCrawler,
	JxbRepo JxbRepo, Cs CCNUServiceProxy, delayQue DelayQueue, refreshLog RefreshLogRepo,
	cf *conf.Server, logger log.Logger) *ClassUsecase {

	waitCrawTime := 1200 * time.Millisecond
	waitUserSvcTime := 10000 * time.Millisecond

	if cf.WaitCrawTime > 0 {
		waitCrawTime = time.Duration(cf.WaitCrawTime) * time.Millisecond
	}
	if cf.WaitUserSvcTime > 0 {
		waitUserSvcTime = time.Duration(cf.WaitUserSvcTime) * time.Millisecond
	}

	cluc := &ClassUsecase{
		classRepo:       classRepo,
		crawler:         crawler,
		jxbRepo:         JxbRepo,
		delayQue:        delayQue,
		ccnu:            Cs,
		refreshLogRepo:  refreshLog,
		waitCrawTime:    waitCrawTime,
		waitUserSvcTime: waitUserSvcTime,
		log:             log.NewHelper(logger),
	}
	// 开启一个协程来处理重试消息
	go func() {
		if err := cluc.delayQue.Consume("be-classlist-refresh-retry", cluc.handleRetryMsg); err != nil {
			cluc.log.Errorf("Error consuming retry message: %v", err)
		}
	}()

	return cluc
}

func (cluc *ClassUsecase) GetClasses(ctx context.Context, stuID, year, semester string, refresh bool) ([]*model.Class, *time.Time, error) {
	var classInfos []*model.ClassInfo

	var wg sync.WaitGroup

	waitCrawTime := cluc.waitCrawTime
	forceNoRefresh := false //强制不刷新

Local: //从本地获取数据

	localResp, err := cluc.classRepo.GetClassesFromLocal(ctx, model.GetClassesFromLocalReq{
		StuID:    stuID,
		Year:     year,
		Semester: semester,
	})

	if err == nil {
		if localResp != nil && len(localResp.ClassInfos) > 0 {
			classInfos = localResp.ClassInfos
		} else {
			err = errors.New("failed to find data in the database")
		}
	} else {
		//这个情况就是从数据库中查询失败了
		//我们只处理数据库中没有数据的情况
		//此时大概率是第一次请求,我们要将等待时间调长
		if errors.Is(err, errcode.ErrClassNotFound) {
			waitCrawTime = max(waitCrawTime, 7*time.Second+500*time.Millisecond)
		}
	}

	//强制不刷新,返回结果
	if forceNoRefresh {
		goto wrapRes
	}

	if refresh || err != nil {

		refreshLog, searchRefreshErr := cluc.refreshLogRepo.SearchRefreshLog(ctx, stuID, year, semester)
		//如果没有报错,说明有记录
		if searchRefreshErr == nil {
			if refreshLog != nil {
				//如果是ready,说明前不久已经爬取过,并且已经更新到数据库了,这里直接返回查询数据库的结果即可
				if refreshLog.IsReady() {
					goto wrapRes
				}

				//如果是pending,说明正在爬取,我们等待一定时间,如果没有结果,则直接返回数据库的结果
				//如果一段时间后是ready,我们重新走数据库
				if refreshLog.IsPending() {
					time.Sleep(cluc.waitCrawTime / 2)
					refreshLog, searchRefreshErr = cluc.refreshLogRepo.GetRefreshLogByID(ctx, refreshLog.ID)
					//这个条件很苛刻,我觉得不太可能走
					if searchRefreshErr != nil || refreshLog == nil {
						goto wrapRes
					}
					// 如果等待一段时间还是pending,说明爬取还没有完成,我们直接返回数据库的结果
					// 或者是failed,我们还是返回数据库的结果，因为我们已经付出了等待的代价
					if refreshLog.IsPending() || refreshLog.IsFailed() {
						goto wrapRes
					}
					//如果有结果,说明爬取完成了,我们再走一遍数据库
					//同时把forceNoRefresh设置为true,防止再走爬虫
					if refreshLog.IsReady() {
						forceNoRefresh = true
						goto Local
					}
				}

				//如果是failed,说明爬取失败,我们重新爬取,走下面的爬取逻辑
			}
		}

		//插入一条log
		logID, insertLogErr := cluc.refreshLogRepo.InsertRefreshLog(ctx, stuID, year, semester)
		if insertLogErr != nil {
			goto wrapRes
		}

		wg.Add(1)

		// 用临时变量接收，避免 data race
		var crawClassInfos []*model.ClassInfo

		// 防止读取和写入并发冲突
		var crawLock sync.Mutex

		go func() {
			//保证wg.Done()只会执行一次
			var once sync.Once
			done := func() {
				once.Do(func() {
					wg.Done()
				})
			}

			// 保证在主协程最多等待waitCrawTime
			time.AfterFunc(waitCrawTime, done)

			defer done()

			crawClassInfos_, crawScs, crawErr := cluc.getCourseFromCrawler(context.Background(), stuID, year, semester)
			if crawErr != nil {
				_ = cluc.refreshLogRepo.UpdateRefreshLogStatus(context.Background(), logID, model.Failed)
				_ = cluc.sendRetryMsg(stuID, year, semester)
				return
			}

			// 确保在赋值前获取锁
			crawLock.Lock()

			// 将数据赋值到闭包外
			crawClassInfos = crawClassInfos_

			// 释放锁
			crawLock.Unlock()

			_, jxbIDs := convertToClass(crawClassInfos)
			saveErr := cluc.classRepo.SaveClass(context.Background(), stuID, year, semester, crawClassInfos_, crawScs)
			//更新log状态
			if saveErr != nil {
				_ = cluc.refreshLogRepo.UpdateRefreshLogStatus(context.Background(), logID, model.Failed)
				_ = cluc.sendRetryMsg(stuID, year, semester)
			} else {
				_ = cluc.refreshLogRepo.UpdateRefreshLogStatus(context.Background(), logID, model.Ready)
			}

			_ = cluc.jxbRepo.SaveJxb(context.Background(), stuID, jxbIDs)
		}()

		var addedClassInfos []*model.ClassInfo

		if refresh {
			addedResp, addedErr := cluc.classRepo.GetAddedClasses(ctx, model.GetAddedClassesReq{
				StudID:   stuID,
				Year:     year,
				Semester: semester,
			})
			if addedErr != nil {
				cluc.log.Warn("failed to find added class in the database")
			}
			if addedResp != nil && len(addedResp.ClassInfos) > 0 {
				addedClassInfos = addedResp.ClassInfos
			}
		}

		wg.Wait()

		// 加锁
		crawLock.Lock()

		// 如果从爬虫中得到了数据，优先用爬虫结果
		if len(crawClassInfos) > 0 {
			classInfos = append(crawClassInfos, addedClassInfos...)
		}

		// 释放锁
		crawLock.Unlock()
	}

wrapRes: //包装结果

	if len(classInfos) == 0 {
		return nil, nil, errcode.ErrClassNotFound
	}

	currentTime := time.Now()
	lastRefreshTime := cluc.refreshLogRepo.GetLastRefreshTime(ctx, stuID, year, semester, currentTime)

	classes, _ := convertToClass(classInfos)

	return classes, lastRefreshTime, nil
}

func (cluc *ClassUsecase) AddClass(ctx context.Context, stuID string, info *model.ClassInfo) error {
	return cluc.addClass(ctx, stuID, info, false)
}

func (cluc *ClassUsecase) DeleteClass(ctx context.Context, stuID, year, semester, classId string) error {
	//删除课程
	err := cluc.classRepo.DeleteClass(ctx, model.DeleteClassReq{
		StuID:    stuID,
		Year:     year,
		Semester: semester,
		ClassId:  []string{classId},
	})
	if err != nil {
		cluc.log.Errorf("delete classlist [%v] failed", classId)
		return errcode.ErrClassDelete
	}
	return nil
}
func (cluc *ClassUsecase) GetRecycledClassInfos(ctx context.Context, stuID, year, semester string) ([]*model.ClassInfo, error) {
	//获取回收站的课程ID
	RecycledClassIds, err := cluc.classRepo.GetRecycledIds(ctx, model.GetRecycledIdsReq{
		StuID:    stuID,
		Year:     year,
		Semester: semester,
	})
	if err != nil {
		return nil, err
	}
	classInfos := make([]*model.ClassInfo, 0)
	//从数据库中查询课程
	for _, classId := range RecycledClassIds.Ids {
		resp, err := cluc.classRepo.GetSpecificClassInfo(ctx, model.GetSpecificClassInfoReq{
			StuID:    stuID,
			Year:     year,
			Semester: semester,
			ClassId:  classId})
		if err != nil {
			continue
		}
		classInfos = append(classInfos, resp.ClassInfo)
	}
	return classInfos, nil
}
func (cluc *ClassUsecase) RecoverClassInfo(ctx context.Context, stuID, year, semester, classId string) error {
	//先检查要回复的课程ID是否存在于回收站中
	exist := cluc.classRepo.CheckClassIdIsInRecycledBin(ctx, model.CheckClassIdIsInRecycledBinReq{
		StuID:    stuID,
		Year:     year,
		Semester: semester,
		ClassId:  classId,
	})
	if !exist {
		return errcode.ErrRecycleBinDoNotHaveIt
	}

	isAdded := cluc.classRepo.IsRecycledCourseManual(ctx, model.IsRecycledCourseManualReq{
		StuID:    stuID,
		Year:     year,
		Semester: semester,
		ClassId:  classId,
	})

	//获取该ID的课程信息
	RecycledClassInfo, err := cluc.SearchClass(ctx, classId)
	if err != nil {
		return errcode.ErrRecover
	}

	//恢复数据库中的对应关系
	err = cluc.addClass(ctx, stuID, RecycledClassInfo, isAdded)
	if err != nil {
		return errcode.ErrRecover
	}

	//删除回收站的对应ID
	err = cluc.classRepo.RemoveClassFromRecycledBin(ctx, model.RemoveClassFromRecycleBinReq{
		StuID:    stuID,
		Year:     year,
		Semester: semester,
		ClassId:  classId,
	})
	if err != nil {
		return errcode.ErrRecover
	}
	return nil
}
func (cluc *ClassUsecase) SearchClass(ctx context.Context, classId string) (*model.ClassInfo, error) {
	resp, err := cluc.classRepo.GetSpecificClassInfo(ctx, model.GetSpecificClassInfoReq{ClassId: classId})
	if err != nil {
		return nil, err
	}
	return resp.ClassInfo, nil
}
func (cluc *ClassUsecase) UpdateClass(ctx context.Context, stuID, year, semester string, newClassInfo *model.ClassInfo, newSc *model.StudentCourse, oldClassId string) error {
	err := cluc.classRepo.UpdateClass(ctx, model.UpdateClassReq{
		StuID:        stuID,
		Year:         year,
		Semester:     semester,
		NewClassInfo: newClassInfo,
		NewSc:        newSc,
		OldClassId:   oldClassId,
	})
	if err != nil {
		return err
	}
	return nil
}
func (cluc *ClassUsecase) CheckSCIdsExist(ctx context.Context, stuID, year, semester, classId string) bool {
	return cluc.classRepo.CheckSCIdsExist(ctx, model.CheckSCIdsExistReq{
		StuID:    stuID,
		Year:     year,
		Semester: semester,
		ClassId:  classId,
	})
}
func (cluc *ClassUsecase) GetAllSchoolClassInfosToOtherService(ctx context.Context, year, semester string, cursor time.Time) []*model.ClassInfo {
	return cluc.classRepo.GetAllSchoolClassInfos(ctx, model.GetAllSchoolClassInfosReq{
		Year:     year,
		Semester: semester,
		Cursor:   cursor,
	}).ClassInfos
}
func (cluc *ClassUsecase) GetStuIdsByJxbId(ctx context.Context, jxbId string) ([]string, error) {
	return cluc.jxbRepo.FindStuIdsByJxbId(ctx, jxbId)
}

func (cluc *ClassUsecase) addClass(ctx context.Context, stuID string, info *model.ClassInfo, isAdded bool) error {
	sc := &model.StudentCourse{
		StuID:           stuID,
		ClaID:           info.ID,
		Year:            info.Year,
		Semester:        info.Semester,
		IsManuallyAdded: isAdded, //手动添加课程
	}
	//检查是否添加的课程是否已经存在
	if cluc.classRepo.CheckSCIdsExist(ctx, model.CheckSCIdsExistReq{StuID: stuID, Year: info.Year, Semester: info.Semester, ClassId: info.ID}) {
		cluc.log.Errorf("[%v] already exists", info)
		return errcode.ErrClassIsExist
	}
	//添加课程
	err := cluc.classRepo.AddClass(ctx, model.AddClassReq{
		StuID:     stuID,
		Year:      info.Year,
		Semester:  info.Semester,
		ClassInfo: info,
		Sc:        sc,
	})
	if err != nil {
		return err
	}
	return nil
}

func (cluc *ClassUsecase) getCourseFromCrawler(ctx context.Context, stuID string, year string, semester string) ([]*model.ClassInfo, []*model.StudentCourse, error) {

	timeoutCtx, cancel := context.WithTimeout(ctx, cluc.waitUserSvcTime) //防止影响
	defer cancel()                                                       // 确保在函数返回前取消上下文，防止资源泄漏

	getCookieStart := time.Now()

	cookie, err := cluc.ccnu.GetCookie(timeoutCtx, stuID)
	if err != nil {
		cluc.log.Errorf("Error getting cookie(stu_id:%v) from other service: %v", stuID, err)
		return nil, nil, err
	}

	cluc.log.Infof("Get cookie (stu_id:%v) from other service,cost %v", stuID, time.Since(getCookieStart))

	var stu Student
	if tool.CheckIsUndergraduate(stuID) { //针对是否是本科生，进行分类
		stu = &Undergraduate{}
	} else {
		stu = &GraduateStudent{}
	}
	classinfos, scs, err := stu.GetClass(ctx, stuID, year, semester, cookie, cluc.crawler)
	if err != nil {
		cluc.log.Errorf("craw classlist(stu_id:%v year:%v semester:%v cookie:%v) failed: %v", stuID, year, semester, cookie, err)
		return nil, nil, err
	}
	return classinfos, scs, nil
}

func convertToClass(infos []*model.ClassInfo) ([]*model.Class, []string) {
	if len(infos) == 0 {
		return nil, nil
	}
	Jxbmp := make(map[string]struct{})
	classes := make([]*model.Class, 0, len(infos))
	for _, classInfo := range infos {
		class := &model.Class{
			Info: classInfo,
		}
		if classInfo.JxbId != "" {
			Jxbmp[classInfo.JxbId] = struct{}{}
		}
		classes = append(classes, class)
	}
	jxbIDs := make([]string, 0, len(Jxbmp))
	for k := range Jxbmp {
		jxbIDs = append(jxbIDs, k)
	}
	return classes, jxbIDs
}

// 发送重试消息
func (cluc *ClassUsecase) sendRetryMsg(stuID, year, semester string) error {
	var retryInfo = map[string]string{
		"stu_id":   stuID,
		"year":     year,
		"semester": semester,
	}
	key := fmt.Sprintf("be-classlist-refresh-retry-%d", time.Now().UnixMilli())
	val, err := json.Marshal(&retryInfo)
	if err != nil {
		return err
	}
	err = cluc.delayQue.Send([]byte(key), val)
	if err != nil {
		cluc.log.Errorf("Error sending retry message: %v", err)
	}
	return err
}

// 处理重试消息
func (cluc *ClassUsecase) handleRetryMsg(key, val []byte) {
	var retryInfo = map[string]string{}

	err := json.Unmarshal(val, &retryInfo)
	if err != nil {
		cluc.log.Errorf("Error unmarshalling retry info: %v", string(val))
		return
	}
	stuID, ok := retryInfo["stu_id"]
	if !ok {
		cluc.log.Errorf("Error getting stu_id from retry info: %v", string(val))
		return
	}
	year, ok := retryInfo["year"]
	if !ok {
		cluc.log.Errorf("Error getting year from retry info: %v", string(val))
		return
	}
	semester, ok := retryInfo["semester"]
	if !ok {
		cluc.log.Errorf("Error getting semester from retry info: %v", string(val))
		return
	}

	//爬取课程信息
	crawClassInfos_, crawScs, crawErr := cluc.getCourseFromCrawler(context.Background(), stuID, year, semester)
	if crawErr != nil {
		cluc.log.Errorf("Error retry getting class info from crawler: %v", crawErr)
		return
	}

	//保存课程信息
	saveErr := cluc.classRepo.SaveClass(context.Background(), stuID, year, semester, crawClassInfos_, crawScs)
	if saveErr != nil {
		cluc.log.Errorf("Error after retry getting class,but saving class info to database: %v", saveErr)
		return
	}

	//插入一条log
	logID, insertLogErr := cluc.refreshLogRepo.InsertRefreshLog(context.Background(), stuID, year, semester)
	if insertLogErr != nil {
		cluc.log.Errorf("Error after retry getting class, but inserting refresh log: %v", insertLogErr)
		return
	}
	//更新日志状态
	_ = cluc.refreshLogRepo.UpdateRefreshLogStatus(context.Background(), logID, model.Ready)
}

// Student 学生接口
type Student interface {
	GetClass(ctx context.Context, stuID, year, semester, cookie string, craw ClassCrawler) ([]*model.ClassInfo, []*model.StudentCourse, error)
}
type Undergraduate struct{}

func (u *Undergraduate) GetClass(ctx context.Context, stuID, year, semester, cookie string, craw ClassCrawler) ([]*model.ClassInfo, []*model.StudentCourse, error) {
	var (
		classInfos = make([]*model.ClassInfo, 0)
		scs        = make([]*model.StudentCourse, 0)
	)
	resp, err := craw.GetClassInfosForUndergraduate(ctx, model.GetClassInfosForUndergraduateReq{
		StuID:    stuID,
		Year:     year,
		Semester: semester,
		Cookie:   cookie,
	})
	if err != nil {
		return nil, nil, err
	}
	if resp != nil {
		if resp.ClassInfos != nil {
			classInfos = resp.ClassInfos
		}
		if resp.StudentCourses != nil {
			scs = resp.StudentCourses
		}
	}
	return classInfos, scs, nil
}

type GraduateStudent struct{}

func (g *GraduateStudent) GetClass(ctx context.Context, stuID, year, semester, cookie string, craw ClassCrawler) ([]*model.ClassInfo, []*model.StudentCourse, error) {
	var (
		classInfos = make([]*model.ClassInfo, 0)
		scs        = make([]*model.StudentCourse, 0)
	)
	resp2, err := craw.GetClassInfoForGraduateStudent(ctx, model.GetClassInfoForGraduateStudentReq{
		StuID:    stuID,
		Year:     year,
		Semester: semester,
		Cookie:   cookie,
	})
	if err != nil {
		return nil, nil, err
	}
	if resp2.ClassInfos != nil {
		classInfos = resp2.ClassInfos
	}
	if resp2.StudentCourses != nil {
		scs = resp2.StudentCourses
	}
	return classInfos, scs, nil
}
