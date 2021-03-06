package sqlancer

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/ast"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/chaos-mesh/go-sqlancer/pkg/connection"
	"github.com/chaos-mesh/go-sqlancer/pkg/executor"
	"github.com/chaos-mesh/go-sqlancer/pkg/generator"
	"github.com/chaos-mesh/go-sqlancer/pkg/knownbugs"
	"github.com/chaos-mesh/go-sqlancer/pkg/transformer"
	. "github.com/chaos-mesh/go-sqlancer/pkg/types"
	. "github.com/chaos-mesh/go-sqlancer/pkg/util"
)

var (
	allColumnTypes = []string{"int", "float", "varchar"}
)

type testingApproach = int

const (
	approachPQS testingApproach = iota
	approachNoREC
	approachTLP
)

type SQLancer struct {
	generator.Generator
	conf     *Config
	executor *executor.Executor

	inWrite      sync.RWMutex
	batch        int
	roundInBatch int
}

// NewSQLancer ...
func NewSQLancer(conf *Config) (*SQLancer, error) {
	log.InitLogger(&log.Config{Level: conf.LogLevel, File: log.FileLogConfig{}})
	e, err := executor.New(conf.DSN, conf.DBName)
	if err != nil {
		return nil, err
	}
	return &SQLancer{
		conf:      conf,
		executor:  e,
		Generator: generator.Generator{Config: generator.Config{Hint: conf.EnableHint}},
	}, nil
}

// Start SQLancer
func (p *SQLancer) Start(ctx context.Context) {
	p.run(ctx)
	p.tearDown()
}

func (p *SQLancer) tearDown() {
	p.executor.Close()
}

// LoadSchema load table/view/index schema
func (p *SQLancer) LoadSchema() {
	rand.Seed(time.Now().UnixNano())
	p.Tables = make([]Table, 0)

	tables, err := p.executor.GetConn().FetchTables(p.conf.DBName)
	if err != nil {
		panic(err)
	}
	for _, i := range tables {
		t := Table{Name: CIStr(i)}
		columns, err := p.executor.GetConn().FetchColumns(p.conf.DBName, i)
		if err != nil {
			panic(err)
		}
		for _, column := range columns {
			col := Column{
				Table: CIStr(i),
				Name:  CIStr(column[0]),
				Null:  strings.EqualFold(column[2], "Yes"),
			}
			col.ParseType(column[1])
			t.Columns = append(t.Columns, col)
		}
		idx, err := p.executor.GetConn().FetchIndexes(p.conf.DBName, i)
		if err != nil {
			panic(err)
		}
		for _, j := range idx {
			t.Indexes = append(t.Indexes, CIStr(j))
		}
		p.Tables = append(p.Tables, t)
	}
}

// setUpDB clears dirty data, creates db, table and populates data
func (p *SQLancer) setUpDB(ctx context.Context) {
	_ = p.executor.Exec("drop database if exists " + p.conf.DBName)
	_ = p.executor.Exec("create database " + p.conf.DBName)
	_ = p.executor.Exec("use " + p.conf.DBName)

	p.createSchema(ctx)
	p.populateData()
	p.createExprIdx()
}

func (p *SQLancer) createSchema(ctx context.Context) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	g, _ := errgroup.WithContext(ctx)
	for index, columnTypes := range ComposeAllColumnTypes(-1, allColumnTypes) {
		tableIndex := index
		colTs := make([]string, len(columnTypes))
		copy(colTs, columnTypes)
		g.Go(func() error {
			sql, _ := p.executor.GenerateDDLCreateTable(tableIndex, colTs)
			return p.executor.Exec(sql.SQLStmt)
		})
	}
	if err := g.Wait(); err != nil {
		log.L().Error("create table failed", zap.Error(err))
	}

	err := p.executor.ReloadSchema()
	if err != nil {
		log.Error("reload data failed!")
	}
	for i := 0; i < r.Intn(10); i++ {
		sql, err := p.executor.GenerateDDLCreateIndex()
		if err != nil {
			log.L().Error("create index error", zap.Error(err))
		}
		err = p.executor.Exec(sql.SQLStmt)
		if err != nil {
			log.L().Error("create index failed", zap.String("sql", sql.SQLStmt), zap.Error(err))
		}
	}
	p.LoadSchema()
}

func (p *SQLancer) populateData() {
	var err error
	if err := p.executor.GetConn().Begin(); err != nil {
		log.L().Error("begin txn failed", zap.Error(err))
		return
	}
	for _, table := range p.executor.GetTables() {
		insertData := func() {
			sql, err := p.executor.GenerateDMLInsertByTable(table.Name.String())
			if err != nil {
				panic(errors.ErrorStack(err))
			}
			err = p.executor.Exec(sql.SQLStmt)
			if err != nil {
				log.L().Error("insert data failed", zap.String("sql", sql.SQLStmt), zap.Error(err))
			}
		}
		insertData()

		// update or delete
		for i := Rd(4); i > 0; i-- {
			tables := p.randTables()

			if err != nil {
				panic(errors.Trace(err))
			}
			if len(tables) == 0 {
				log.L().Panic("tables random by ChoosePivotedRow is empty")
			}
			var dmlStmt string
			switch Rd(2) {
			case 0:
				dmlStmt, err = p.DeleteStmt(tables, *table)
				if err != nil {
					// TODO: goto next generation
					log.L().Error("generate delete stmt failed", zap.Error(err))
				}
			default:
				dmlStmt, err = p.UpdateStmt(tables, *table)
				if err != nil {
					// TODO: goto next generation
					log.L().Error("generate update stmt failed", zap.Error(err))
				}
			}
			log.L().Info("Update/Delete statement", zap.String(table.Name.String(), dmlStmt))
			err = p.executor.Exec(dmlStmt)
			if err != nil {
				log.L().Error("update/delete data failed", zap.String("sql", dmlStmt), zap.Error(err))
				panic(err)
			}
		}

		countSQL := "select count(*) from " + table.Name.String()
		qi, err := p.executor.GetConn().Select(countSQL)
		if err != nil {
			log.L().Error("insert data failed", zap.String("sql", countSQL), zap.Error(err))
		}
		count := qi[0][0].ValString
		log.L().Debug("table check records count", zap.String(table.Name.String(), count))
		if c, _ := strconv.ParseUint(count, 10, 64); c == 0 {
			log.L().Info(table.Name.String() + " is empty after DELETE")
			insertData()
		}
	}
	if err := p.executor.GetConn().Commit(); err != nil {
		log.L().Error("commit txn failed", zap.Error(err))
		return
	}
}

func (p *SQLancer) createExprIdx() {
	if p.conf.EnableExprIndex {
		p.addExprIndex()
		// reload indexes created
		p.LoadSchema()
	}
}

func (p *SQLancer) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if p.roundInBatch == 0 {
				p.refreshDatabase(ctx)
				p.batch++
			}
			p.progress()
			p.roundInBatch = (p.roundInBatch + 1) % 100
		}
	}
}

func (p *SQLancer) progress() {
	p.inWrite.RLock()
	defer func() {
		p.inWrite.RUnlock()
	}()
	var approaches []testingApproach
	// Because we creates view just in time with process(we creates views on first ViewCount rounds)
	// and current implementation depends on the PQS to ensures that there exists at lease one row in that view
	// so we must choose approachPQS in this scenario
	if p.roundInBatch < p.conf.ViewCount {
		approaches = []testingApproach{approachPQS}
	} else {
		if p.conf.EnableNoRECApproach {
			approaches = append(approaches, approachNoREC)
		}
		if p.conf.EnablePQSApproach {
			approaches = append(approaches, approachPQS)
		}
		if p.conf.EnableTLPApproach {
			approaches = append(approaches, approachTLP)
		}
	}
	approach := approaches[Rd(len(approaches))]
	switch approach {
	case approachPQS:
		// rand one pivot row for one table
		pivotRows, usedTables, err := p.ChoosePivotedRow()
		if err != nil {
			log.L().Fatal("choose pivot row failed", zap.Error(err))
		}
		selectAST, selectSQL, columns, pivotRows, err := p.GenPQSSelectStmt(pivotRows, usedTables)
		p.withTxn(RdBool(), func() error {
			resultRows, err := p.execSelect(selectSQL)
			if err != nil {
				log.L().Error("execSelect failed", zap.Error(err))
				return err
			}
			correct := p.verifyPQS(pivotRows, columns, resultRows)
			if !correct {
				// subSQL, err := p.minifySelect(selStmt, pivotRows, usedTables, columns)
				// if err != nil {
				// 	log.Error("occurred an error when try to simplify select", zap.String("sql", selectSQL), zap.Error(err))
				// 	fmt.Printf("query:\n%s\n", selectSQL)
				// } else {
				// 	fmt.Printf("query:\n%s\n", selectSQL)
				// 	if len(subSQL) < len(selectSQL) {
				// 		fmt.Printf("sub query:\n%s\n", subSQL)
				// 	}
				// }
				dust := knownbugs.NewDustbin([]ast.Node{selectAST}, pivotRows)
				if dust.IsKnownBug() {
					return nil
				}
				fmt.Printf("row:\n")
				p.printPivotRows(pivotRows)
				if p.roundInBatch < p.conf.ViewCount || p.conf.Silent {
					panic("data verified failed")
				}
				return nil
			}
			if p.roundInBatch <= p.conf.ViewCount {
				if err := p.executor.GetConn().CreateViewBySelect(fmt.Sprintf("view_%d", p.roundInBatch), selectSQL, len(resultRows), columns); err != nil {
					log.L().Error("create view failed", zap.Error(err))
				}
			}
			if p.roundInBatch == p.conf.ViewCount {
				p.LoadSchema()
				if err := p.executor.ReloadSchema(); err != nil {
					panic(err)
				}
			}
			log.L().Info("check finished", zap.String("approach", "PQS"), zap.Int("batch", p.batch), zap.Int("round", p.roundInBatch), zap.Bool("result", correct))
			return nil
		})
	case approachNoREC, approachTLP:
		selectAst, _, genCtx, err := p.GenSelectStmt()
		if err != nil {
			log.L().Error("generate normal SQL statement failed", zap.Error(err))
		}
		var transformers []transformer.Transformer
		if approach == approachNoREC {
			transformers = []transformer.Transformer{transformer.NoREC}
		} else {
			transformers = []transformer.Transformer{
				// approachTLP contains transformer.NoREC
				transformer.NoREC,
				&transformer.TLPTrans{
					Expr: &ast.ParenthesesExpr{Expr: p.ConditionClause(genCtx, 2)},
					Tp:   transformer.WHERE,
				}}
		}
		p.withTxn(RdBool(), func() error {
			nodesGroup := transformer.Transform(transformers, selectAst, 3)
			for _, nodesArr := range nodesGroup {
				if len(nodesArr) < 2 {
					sql, _ := BufferOut(selectAst)
					log.L().Warn("no enough sqls were generated", zap.String("error sql", sql), zap.Int("node length", len(nodesArr)))
					continue
				}
				sqlInOneGroup := make([]string, 0)
				resultSet := make([][][]*connection.QueryItem, 0)
				for _, node := range nodesArr {
					sql, err := BufferOut(node)
					if err != nil {
						log.L().Error("err on restoring", zap.Error(err))
					} else {
						resultRows, err := p.execSelect(sql)
						log.L().Warn(sql)
						if err != nil {
							log.L().Error("execSelect failed", zap.Error(err))
							return err
						}
						resultSet = append(resultSet, resultRows)
					}
					sqlInOneGroup = append(sqlInOneGroup, sql)
				}
				correct := p.checkResultSet(resultSet, true)
				if !correct {
					log.L().Error("last round SQLs", zap.Strings("", sqlInOneGroup))
					if !p.conf.Silent {
						log.L().Fatal("data verified failed")
					}
				}
				log.L().Info("check finished", zap.String("approach", "NoREC"), zap.Int("batch", p.batch), zap.Int("round", p.roundInBatch), zap.Bool("result", correct))
			}
			return nil
		})
	default:
		log.L().Fatal("unknown check approach", zap.Int("approach", approach))
	}
}

// if useExplicitTxn is set, a explicit transaction is used when doing action
// otherwise, uses auto-commit
func (p *SQLancer) withTxn(useExplicitTxn bool, action func() error) error {
	var err error
	// execute sql, ensure not null result set
	if useExplicitTxn {
		if err = p.executor.GetConn().Begin(); err != nil {
			log.L().Error("begin txn failed", zap.Error(err))
			return err
		}
		log.L().Debug("begin txn success")
		defer func() {
			if err = p.executor.GetConn().Commit(); err != nil {
				log.L().Error("commit txn failed", zap.Error(err))
				return
			}
			log.L().Debug("commit txn success")
		}()
	}
	return action()
}

func (p *SQLancer) addExprIndex() {
	for i := 0; i < Rd(10)+1; i++ {
		n := p.createExpressionIndex()
		if n == nil {
			continue
		}
		var sql string
		if sql, err := BufferOut(n); err != nil {
			// should never panic
			panic(errors.Trace(err))
		} else if _, err = p.executor.GetConn().Select(sql); err != nil {
			panic(errors.Trace(err))
		}
		fmt.Println("add one index on expression success SQL:" + sql)
	}
	fmt.Println("Create expression index successfully")
}

func (p *SQLancer) createExpressionIndex() *ast.CreateIndexStmt {
	table := p.Tables[Rd(len(p.Tables))]
	/* only contains a primary key col and a varchar col in `table_varchar`
	   it will cause panic when create an expression index on it
	   since a varchar can not do logic ops and other ops with numberic
	*/
	if table.Name.EqString("table_varchar") {
		return nil
	}
	columns := make([]Column, 0)
	// remove auto increment column for avoiding ERROR 3109:
	// `Generated column '' cannot refer to auto-increment column`
	for _, column := range table.Columns {
		if !column.Name.HasPrefix("id_") {
			columns = append(columns, column)
		}
	}
	var backup []Column
	copy(backup, table.Columns)
	table.Columns = columns

	exprs := make([]ast.ExprNode, 0)
	for x := 0; x < Rd(3)+1; x++ {
		gCtx := generator.NewGenCtx([]Table{table}, nil)
		gCtx.IsInExprIndex = true
		gCtx.EnableLeftRightJoin = false
		exprs = append(exprs, &ast.ParenthesesExpr{Expr: p.ConditionClause(gCtx, 1)})
	}
	node := ast.CreateIndexStmt{}
	node.IndexName = "idx_" + RdStringChar(5)
	node.Table = &ast.TableName{Name: table.Name.ToModel()}
	node.IndexPartSpecifications = make([]*ast.IndexPartSpecification, 0)
	for _, expr := range exprs {
		node.IndexPartSpecifications = append(node.IndexPartSpecifications, &ast.IndexPartSpecification{
			Expr: expr,
		})
	}
	node.IndexOption = &ast.IndexOption{}

	table.Columns = backup
	return &node
}

func (p *SQLancer) randTables() []Table {
	count := 1
	if len(p.Tables) > 1 {
		// avoid too deep joins
		if count = Rd(len(p.Tables)-1) + 1; count > 4 {
			count = Rd(4) + 1
		}
	}
	rand.Shuffle(len(p.Tables), func(i, j int) { p.Tables[i], p.Tables[j] = p.Tables[j], p.Tables[i] })
	usedTables := make([]Table, count)
	copy(usedTables, p.Tables[:count])
	return usedTables
}

// ChoosePivotedRow choose a row
// it may move to another struct
func (p *SQLancer) ChoosePivotedRow() (map[string]*connection.QueryItem, []Table, error) {
	result := make(map[string]*connection.QueryItem)
	usedTables := p.randTables()
	var reallyUsed []Table

	for _, i := range usedTables {
		sql := fmt.Sprintf("SELECT * FROM %s ORDER BY RAND() LIMIT 1;", i.Name)
		exeRes, err := p.execSelect(sql)
		if err != nil {
			panic(err)
		}
		if len(exeRes) > 0 {
			for _, c := range exeRes[0] {
				// panic(fmt.Sprintf("no rows in table %s", i.Column))
				tableColumn := Column{Table: i.Name, Name: CIStr(c.ValType.Name())}
				result[tableColumn.String()] = c
			}
			reallyUsed = append(reallyUsed, i)

		}
	}
	return result, reallyUsed, nil
}

func (p *SQLancer) GenPQSSelectStmt(pivotRows map[string]*connection.QueryItem,
	usedTables []Table) (*ast.SelectStmt, string, []Column, map[string]*connection.QueryItem, error) {
	genCtx := generator.NewGenCtx(usedTables, pivotRows)
	genCtx.IsPQSMode = true

	return p.Generator.SelectStmt(genCtx, p.conf.Depth)
}

func (p *SQLancer) ExecAndVerify(stmt *ast.SelectStmt, originRow map[string]*connection.QueryItem, columns []Column) (bool, error) {
	sql, err := BufferOut(stmt)
	if err != nil {
		return false, err
	}
	resultSets, err := p.execSelect(sql)
	if err != nil {
		return false, err
	}
	res := p.verifyPQS(originRow, columns, resultSets)
	return res, nil
}

// may not return string
func (p *SQLancer) execSelect(stmt string) ([][]*connection.QueryItem, error) {
	log.L().Debug("execSelect", zap.String("stmt", stmt))
	return p.executor.GetConn().Select(stmt)
}

func (p *SQLancer) verifyPQS(originRow map[string]*connection.QueryItem, columns []Column, resultSets [][]*connection.QueryItem) bool {
	for _, row := range resultSets {
		if p.checkRow(originRow, columns, row) {
			return true
		}
	}
	return false
}

func (p *SQLancer) checkRow(originRow map[string]*connection.QueryItem, columns []Column, resultSet []*connection.QueryItem) bool {
	for i, c := range columns {
		// fmt.Printf("i: %d, column: %+v, left: %+v, right: %+v", i, c, originRow[c], resultSet[i])
		if !compareQueryItem(originRow[c.GetAliasName().String()], resultSet[i]) {
			return false
		}
	}
	return true
}

func (p *SQLancer) printPivotRows(pivotRows map[string]*connection.QueryItem) {
	var tableColumns Columns
	for column := range pivotRows {
		parsed := strings.Split(column, ".")
		table, col := parsed[0], parsed[1]
		tableColumns = append(tableColumns, Column{
			Table: CIStr(table),
			Name:  CIStr(col),
		})
	}

	sort.Sort(tableColumns)
	for _, column := range tableColumns {
		value := pivotRows[column.String()]
		fmt.Printf("%s.%s=%s\n", column.Table, column.Name, value.String())
	}
}

func compareQueryItem(left *connection.QueryItem, right *connection.QueryItem) bool {
	// if left.ValType.Name() != right.ValType.Name() {
	// 	return false
	// }
	if left.Null != right.Null {
		return false
	}

	return (left.Null && right.Null) || (left.ValString == right.ValString)
}

func (p *SQLancer) refreshDatabase(ctx context.Context) {
	p.inWrite.Lock()
	defer func() {
		p.inWrite.Unlock()
	}()
	log.L().Debug("refresh database")
	p.setUpDB(ctx)
}

func (p *SQLancer) GenSelectStmt() (*ast.SelectStmt, string, *generator.GenCtx, error) {
	genCtx := generator.NewGenCtx(p.randTables(), nil)
	genCtx.IsPQSMode = false

	selectAST, selectSQL, _, _, err := p.Generator.SelectStmt(genCtx, p.conf.Depth)
	return selectAST, selectSQL, genCtx, err
}

func (p *SQLancer) checkResultSet(set [][][]*connection.QueryItem, ignoreSort bool) bool {
	if len(set) < 2 {
		return true
	}

	// TODO: now only compare result rows number
	// should support to compare rows' order

	length := len(set[0])
	for _, rows := range set {
		if len(rows) != length {
			return false
		}
	}
	return true
}
