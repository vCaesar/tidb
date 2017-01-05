// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package plan

import (
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/mysql"
)

// Columns of an aggregation's schema is an bijection with aggregation's aggFuncs.
// Sometimes we need to convert from one side to another, then this function is needed.
func (p *Aggregation) buildSchemaByAggFuncs() expression.Schema {
	schema := expression.NewSchema(make([]*expression.Column, 0, p.schema.Len()))
	for _, fun := range p.AggFuncs {
		if col, isCol := fun.GetArgs()[0].(*expression.Column); isCol && fun.GetName() == ast.AggFuncFirstRow {
			schema.Append(col)
		} else {
			// If the arg is not a column, we add a column to occupy the position.
			schema.Append(&expression.Column{
				FromID:   "",
				Position: -1})
		}
	}
	return schema
}

func (p *Aggregation) buildKeyInfo() {
	p.baseLogicalPlan.buildKeyInfo()
	// Dealing with p.AggFuncs.
	schemaByFuncs := p.buildSchemaByAggFuncs()
	for _, fun := range p.AggFuncs {
		if col, isCol := fun.GetArgs()[0].(*expression.Column); isCol && fun.GetName() == ast.AggFuncFirstRow {
			schemaByFuncs.Append(col)
		} else {
			// If the arg is not a column, we add a column to occupy the position.
			schemaByFuncs.Append(&expression.Column{
				FromID:   "",
				Position: -1})
		}
	}
	for _, key := range p.GetChildren()[0].GetSchema().Keys {
		indices, ok := schemaByFuncs.GetColumnsIndices(key)
		if !ok {
			continue
		}
		newKey := make([]*expression.Column, 0, len(key))
		for _, i := range indices {
			newKey = append(newKey, p.schema.Columns[i])
		}
		p.schema.Keys = append(p.schema.Keys, newKey)
	}
	// Dealing with p.GroupbyCols
	// This is only used for optimization and needn't to be pushed up, so only one is enough.
	schemaByGroupby := expression.NewSchema(p.groupByCols)
	for _, key := range p.GetChildren()[0].GetSchema().Keys {
		indices, ok := schemaByGroupby.GetColumnsIndices(key)
		if !ok {
			continue
		}
		newKey := make([]*expression.Column, 0, len(key))
		for _, i := range indices {
			newKey = append(newKey, schemaByGroupby.Columns[i])
		}
		p.schema.Keys = append(p.schema.Keys, newKey)
		break
	}
}

// Columns of a projection's schema is an bijection with projection's Exprs.
// Sometimes we need to convert from one side to another, then this function is needed.
func (p *Projection) buildSchemaByExprs() expression.Schema {
	schema := expression.NewSchema(make([]*expression.Column, 0, p.schema.Len()))
	for _, expr := range p.Exprs {
		if col, isCol := expr.(*expression.Column); isCol {
			schema.Append(col)
		} else {
			// If the expression is not a column, we add a column to occupy the position.
			schema.Append(&expression.Column{
				FromID:   "",
				Position: -1})
		}
	}
	return schema
}

func (p *Projection) buildKeyInfo() {
	p.baseLogicalPlan.buildKeyInfo()
	schema := p.buildSchemaByExprs()
	for _, key := range p.GetChildren()[0].GetSchema().Keys {
		indices, ok := schema.GetColumnsIndices(key)
		if !ok {
			continue
		}
		newKey := make([]*expression.Column, 0, len(key))
		for _, i := range indices {
			newKey = append(newKey, p.schema.Columns[i])
		}
		p.schema.Keys = append(p.schema.Keys, newKey)
	}
}

func (p *Trim) buildKeyInfo() {
	p.baseLogicalPlan.buildKeyInfo()
	for _, key := range p.children[0].GetSchema().Keys {
		ok := false
		newKey := make([]*expression.Column, 0, len(key))
		for _, col := range key {
			pos := p.schema.GetColumnIndex(col)
			if pos == -1 {
				ok = false
				break
			}
			newKey = append(newKey, p.schema.Columns[pos])
		}
		if ok {
			p.schema.Keys = append(p.schema.Keys, newKey)
		}
	}
}

func (p *Join) buildKeyInfo() {
	p.baseLogicalPlan.buildKeyInfo()
	switch p.JoinType {
	case SemiJoin, SemiJoinWithAux:
		p.schema.Keys = p.children[0].GetSchema().Clone().Keys
	case InnerJoin, LeftOuterJoin, RightOuterJoin:
		// We first check the equal conditions. If one side of one equal condition is unique key,
		// another side's unique key information will all be reserved.
		if len(p.EqualConditions) == 0 {
			return
		}
		lOk := false
		rOk := false
		for _, expr := range p.EqualConditions {
			ln := expr.GetArgs()[0].(*expression.Column)
			rn := expr.GetArgs()[1].(*expression.Column)
			for _, key := range p.children[0].GetSchema().Keys {
				if len(key) == 1 && key[0].Equal(ln, p.ctx) {
					lOk = true
					break
				}
			}
			for _, key := range p.children[1].GetSchema().Keys {
				if len(key) == 1 && key[0].Equal(rn, p.ctx) {
					rOk = true
					break
				}
			}
		}
		if lOk {
			p.schema.Keys = append(p.schema.Keys, p.children[1].GetSchema().Keys...)
		}
		if rOk {
			p.schema.Keys = append(p.schema.Keys, p.children[0].GetSchema().Keys...)
		}
	}
}

func (p *DataSource) buildKeyInfo() {
	p.baseLogicalPlan.buildKeyInfo()
	indices, _ := availableIndices(p.indexHints, p.tableInfo)
	for _, idx := range indices {
		if !idx.Unique {
			continue
		}
		newKey := make([]*expression.Column, 0, len(idx.Columns))
		ok := true
		for _, idxCol := range idx.Columns {
			// The columns of this index should all occur in column schema.
			find := false
			for i, col := range p.schema.Columns {
				if idxCol.Name.L == col.ColName.L {
					newKey = append(newKey, p.schema.Columns[i])
					find = true
					break
				}
			}
			if !find {
				ok = false
				break
			}
		}
		if ok {
			p.schema.Keys = append(p.schema.Keys, newKey)
		}
	}
	if p.tableInfo.PKIsHandle {
		for i, col := range p.Columns {
			if mysql.HasPriKeyFlag(col.Flag) {
				p.schema.Keys = append(p.schema.Keys, []*expression.Column{p.schema.Columns[i]})
				break
			}
		}
	}
}

func (p *Apply) buildKeyInfo() {
	p.baseLogicalPlan.buildKeyInfo()
	p.schema.Keys = append(p.children[0].GetSchema().Clone().Keys, p.children[1].GetSchema().Clone().Keys...)
}
