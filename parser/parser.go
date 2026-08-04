package parser

import (
	"fmt"
	"strconv"
	"strings"
)

type Parser struct {
	tokens   []Token
	position int
	source   []rune
}

func Parse(sql string) (Statement, error) {
	expanded, err := ExpandMySQLExecutableComments(sql)
	if err != nil {
		return nil, err
	}
	sql = expanded
	tokens, err := Lex(sql)
	if err != nil {
		return nil, err
	}
	p := &Parser{tokens: tokens, source: []rune(sql)}
	statement, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	p.acceptKind(TokenSemicolon)
	if p.current().Kind != TokenEOF {
		return nil, p.errorf("unexpected token %q", p.current().Text)
	}
	return statement, nil
}

func ParseExpression(sql string) (Expr, error) {
	tokens, err := Lex(sql)
	if err != nil {
		return nil, err
	}
	p := &Parser{tokens: tokens, source: []rune(sql)}
	expression, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if p.current().Kind != TokenEOF {
		return nil, p.errorf("unexpected token %q in expression", p.current().Text)
	}
	return expression, nil
}

func (p *Parser) parseStatement() (Statement, error) {
	switch {
	case p.current().Kind == TokenEOF || p.current().Kind == TokenSemicolon:
		return Empty{}, nil
	case p.accept("CREATE"):
		return p.parseCreate()
	case p.accept("DROP"):
		return p.parseDrop()
	case p.accept("INSERT"):
		return p.parseInsert(false)
	case p.accept("REPLACE"):
		return p.parseInsert(true)
	case p.accept("SELECT"):
		return p.parseUnionSelect()
	case p.accept("EXPLAIN"):
		if err := p.expect("SELECT"); err != nil {
			return nil, err
		}
		query, err := p.parseUnionSelect()
		if err != nil {
			return nil, err
		}
		return Explain{Query: query}, nil
	case p.accept("WITH"):
		return p.parseWith()
	case p.accept("TRUNCATE"):
		p.accept("TABLE")
		table, err := p.identifier()
		return Truncate{Table: table}, err
	case p.accept("DESCRIBE") || p.accept("DESC"):
		table, err := p.identifier()
		return Show{What: "COLUMNS", Name: table}, err
	case p.accept("UPDATE"):
		return p.parseUpdate()
	case p.accept("DELETE"):
		return p.parseDelete()
	case p.accept("SHOW"):
		return p.parseShow()
	case p.accept("USE"):
		name, err := p.identifier()
		return Use{Database: name}, err
	case p.accept("BEGIN") || p.accept("START"):
		p.accept("TRANSACTION")
		return Begin{}, nil
	case p.accept("COMMIT"):
		return Commit{}, nil
	case p.accept("ROLLBACK"):
		return Rollback{}, nil
	case p.accept("ALTER"):
		return p.parseAlter()
	case p.accept("RENAME"):
		return p.parseRename()
	case p.accept("SET"):
		return p.parseSet()
	case p.accept("GRANT"):
		return p.parseGrant()
	case p.accept("REVOKE"):
		return p.parseRevoke()
	case p.accept("EXPORT"):
		return p.parseExport()
	default:
		return nil, p.errorf("unsupported statement %q", p.current().Text)
	}
}

func (p *Parser) parseUnionSelect() (Query, error) {
	first, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	left := first.(Select)
	if !p.is("UNION") {
		return left, nil
	}
	statement := Union{Queries: []Select{left}}
	for p.accept("UNION") {
		all := p.accept("ALL")
		if !all {
			p.accept("DISTINCT")
		}
		if err := p.expect("SELECT"); err != nil {
			return nil, err
		}
		next, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		statement.All = append(statement.All, all)
		statement.Queries = append(statement.Queries, next.(Select))
	}
	last := &statement.Queries[len(statement.Queries)-1]
	statement.OrderBy = last.OrderBy
	statement.Limit, statement.Offset, statement.HasLimit = last.Limit, last.Offset, last.HasLimit
	last.OrderBy = nil
	last.Limit, last.Offset, last.HasLimit = 0, 0, false
	return statement, nil
}

func (p *Parser) parseWith() (Statement, error) {
	if !p.accept("RECURSIVE") {
		statement := With{}
		for {
			name, err := p.identifier()
			if err != nil {
				return nil, err
			}
			expression := CommonTableExpression{Name: name}
			if p.acceptKind(TokenLParen) {
				for {
					column, err := p.identifier()
					if err != nil {
						return nil, err
					}
					expression.Columns = append(expression.Columns, column)
					if !p.acceptKind(TokenComma) {
						break
					}
				}
				if err := p.expectKind(TokenRParen, ")"); err != nil {
					return nil, err
				}
			}
			if err := p.expect("AS"); err != nil {
				return nil, err
			}
			if err := p.expectKind(TokenLParen, "("); err != nil {
				return nil, err
			}
			if err := p.expect("SELECT"); err != nil {
				return nil, err
			}
			expression.Query, err = p.parseUnionSelect()
			if err != nil {
				return nil, err
			}
			if err := p.expectKind(TokenRParen, ")"); err != nil {
				return nil, err
			}
			statement.Expressions = append(statement.Expressions, expression)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
		if err := p.expect("SELECT"); err != nil {
			return nil, err
		}
		query, err := p.parseUnionSelect()
		if err != nil {
			return nil, err
		}
		statement.Query = query
		return statement, nil
	}
	name, err := p.identifier()
	if err != nil {
		return nil, err
	}
	if err := p.expect("AS"); err != nil {
		return nil, err
	}
	if err := p.expectKind(TokenLParen, "("); err != nil {
		return nil, err
	}
	if err := p.expect("SELECT"); err != nil {
		return nil, err
	}
	seedStatement, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	seed, ok := seedStatement.(Select)
	if !ok {
		return nil, p.errorf("CTE seed must be a SELECT")
	}
	if err := p.expect("UNION"); err != nil {
		return nil, err
	}
	if err := p.expect("ALL"); err != nil {
		return nil, err
	}
	if err := p.expect("SELECT"); err != nil {
		return nil, err
	}
	recursiveStatement, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	recursive, ok := recursiveStatement.(Select)
	if !ok {
		return nil, p.errorf("CTE recursive term must be a SELECT")
	}
	if err := p.expectKind(TokenRParen, ")"); err != nil {
		return nil, err
	}
	if err := p.expect("SELECT"); err != nil {
		return nil, err
	}
	queryStatement, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	query, ok := queryStatement.(Select)
	if !ok {
		return nil, p.errorf("CTE body must be a SELECT")
	}
	return WithRecursive{Name: name, Seed: seed, Recursive: recursive, Query: query}, nil
}

func (p *Parser) parseCreate() (Statement, error) {
	orReplace := false
	if p.accept("OR") {
		if err := p.expect("REPLACE"); err != nil {
			return nil, err
		}
		orReplace = true
	}
	if orReplace || p.is("VIEW") || p.is("ALGORITHM") || p.is("DEFINER") || p.is("SQL") {
		return p.parseCreateView(orReplace, false)
	}
	if p.accept("DATABASE") || p.accept("SCHEMA") {
		ifNotExists := false
		if p.accept("IF") {
			if err := p.expect("NOT"); err != nil {
				return nil, err
			}
			if err := p.expect("EXISTS"); err != nil {
				return nil, err
			}
			ifNotExists = true
		}
		name, err := p.identifier()
		if err != nil {
			return nil, err
		}
		if err := p.parseDatabaseOptions(); err != nil {
			return nil, err
		}
		return CreateDatabase{Name: name, IfNotExists: ifNotExists}, nil
	}
	if p.accept("TABLE") {
		ifNotExists := false
		if p.accept("IF") {
			if err := p.expect("NOT"); err != nil {
				return nil, err
			}
			if err := p.expect("EXISTS"); err != nil {
				return nil, err
			}
			ifNotExists = true
		}
		name, err := p.identifier()
		if err != nil {
			return nil, err
		}
		if p.accept("LIKE") {
			source, sourceErr := p.identifier()
			return CreateTableLike{Name: name, Source: source, IfNotExists: ifNotExists}, sourceErr
		}
		as := p.accept("AS")
		if as || p.accept("SELECT") {
			if as {
				if err := p.expect("SELECT"); err != nil {
					return nil, err
				}
			}
			query, queryErr := p.parseUnionSelect()
			return CreateTableAs{Name: name, IfNotExists: ifNotExists, Query: query}, queryErr
		}
		if err := p.expectKind(TokenLParen, "("); err != nil {
			return nil, err
		}
		var columns []ColumnDef
		var primaryKey []string
		var indexes []IndexDef
		var foreignKeys []ForeignKeyDef
		var checks []CheckDef
		for !p.acceptKind(TokenRParen) {
			if p.isTableConstraint() {
				if p.accept("PRIMARY") {
					if err := p.expect("KEY"); err != nil {
						return nil, err
					}
					primaryKey, err = p.parseIndexColumns()
					if err != nil {
						return nil, err
					}
					p.parseIndexMethod()
				} else if p.is("UNIQUE") || p.is("KEY") || p.is("INDEX") {
					unique := p.accept("UNIQUE")
					p.accept("KEY")
					p.accept("INDEX")
					index, err := p.parseTableIndexDefinition(unique)
					if err != nil {
						return nil, err
					}
					indexes = append(indexes, index)
				} else if p.is("CONSTRAINT") || p.is("FOREIGN") || p.is("CHECK") {
					constraintName := ""
					if p.accept("CONSTRAINT") {
						constraintName, err = p.identifier()
						if err != nil {
							return nil, err
						}
					}
					if p.accept("CHECK") {
						expression, checkErr := p.parseParenthesizedDefinition()
						if checkErr != nil {
							return nil, checkErr
						}
						checks = append(checks, CheckDef{Name: constraintName, Expression: expression})
					} else {
						foreignKey, foreignKeyErr := p.parseForeignKeyDefinition(constraintName)
						if foreignKeyErr != nil {
							return nil, foreignKeyErr
						}
						foreignKeys = append(foreignKeys, foreignKey)
					}
				} else {
					p.skipDefinitionTail()
				}
				if p.acceptKind(TokenComma) {
					continue
				}
				if err := p.expectKind(TokenRParen, ")"); err != nil {
					return nil, err
				}
				break
			}
			column, err := p.parseColumnDefinition()
			if err != nil {
				return nil, err
			}
			columns = append(columns, column)
			if column.Check != "" {
				checks = append(checks, CheckDef{Expression: column.Check})
			}
			if column.PrimaryKey {
				primaryKey = append(primaryKey, column.Name)
			}
			p.skipDefinitionTail()
			if p.acceptKind(TokenComma) {
				continue
			}
			if err := p.expectKind(TokenRParen, ")"); err != nil {
				return nil, err
			}
			break
		}
		comment := ""
		for p.current().Kind != TokenEOF && p.current().Kind != TokenSemicolon {
			if p.accept("COMMENT") {
				if p.current().Kind == TokenOperator && p.current().Text == "=" {
					p.position++
				}
				if p.current().Kind != TokenString {
					return nil, p.errorf("expected table comment")
				}
				comment = p.current().Text
				p.position++
				continue
			}
			p.position++
		}
		for _, column := range columns {
			if column.Unique {
				indexes = append(indexes, IndexDef{Name: "uq_" + column.Name, Columns: []string{column.Name}, Unique: true})
			}
		}
		return CreateTable{Name: name, Columns: columns, PrimaryKey: primaryKey, Indexes: indexes, IfNotExists: ifNotExists, Comment: comment, ForeignKeys: foreignKeys, Checks: checks}, nil
	}
	unique := p.accept("UNIQUE")
	if p.accept("INDEX") || p.accept("KEY") {
		name, err := p.identifier()
		if err != nil {
			return nil, err
		}
		if err := p.expect("ON"); err != nil {
			return nil, err
		}
		table, err := p.identifier()
		if err != nil {
			return nil, err
		}
		columns, err := p.parseIndexColumns()
		if err != nil {
			return nil, err
		}
		p.parseIndexMethod()
		return CreateIndex{Name: name, Table: table, Columns: columns, Unique: unique}, nil
	}
	if unique {
		return nil, p.errorf("expected INDEX or KEY after UNIQUE")
	}
	if p.accept("USER") {
		ifNotExists, err := p.parseIfNotExists()
		if err != nil {
			return nil, err
		}
		users, err := p.parseUserSpecs(false)
		return CreateUser{Users: users, IfNotExists: ifNotExists}, err
	}
	return nil, p.errorf("expected DATABASE, TABLE, VIEW, INDEX, or USER")
}

func (p *Parser) parseCreateView(orReplace, alterOnly bool) (Statement, error) {
	hasCreateOptions := false
	for {
		switch {
		case p.accept("ALGORITHM"):
			hasCreateOptions = true
			if p.current().Kind == TokenOperator && p.current().Text == "=" {
				p.position++
			}
			if p.current().Kind != TokenIdentifier {
				return nil, p.errorf("expected view algorithm")
			}
			p.position++
		case p.accept("DEFINER"):
			hasCreateOptions = true
			if p.current().Kind == TokenOperator && p.current().Text == "=" {
				p.position++
			}
			for !p.is("SQL") && !p.is("VIEW") && p.current().Kind != TokenEOF {
				p.position++
			}
		case p.accept("SQL"):
			hasCreateOptions = true
			if err := p.expect("SECURITY"); err != nil {
				return nil, err
			}
			if !p.accept("DEFINER") && !p.accept("INVOKER") {
				return nil, p.errorf("expected DEFINER or INVOKER")
			}
		default:
			goto optionsDone
		}
	}

optionsDone:
	if err := p.expect("VIEW"); err != nil {
		return nil, err
	}
	name, err := p.identifier()
	if err != nil {
		return nil, err
	}
	var columns []string
	if p.acceptKind(TokenLParen) {
		for {
			column, err := p.identifier()
			if err != nil {
				return nil, err
			}
			columns = append(columns, column)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
		if err := p.expectKind(TokenRParen, ")"); err != nil {
			return nil, err
		}
	}
	if err := p.expect("AS"); err != nil {
		return nil, err
	}
	definitionStart := p.position
	if err := p.expect("SELECT"); err != nil {
		return nil, err
	}
	if _, err := p.parseUnionSelect(); err != nil {
		return nil, err
	}
	definitionEnd := p.position
	if p.accept("WITH") {
		p.accept("CASCADED")
		p.accept("LOCAL")
		if err := p.expect("CHECK"); err != nil {
			return nil, err
		}
		if err := p.expect("OPTION"); err != nil {
			return nil, err
		}
	}
	definition := joinTokens(p.tokens[definitionStart:definitionEnd])
	if len(p.source) > 0 {
		startPosition := p.tokens[definitionStart].Position
		endPosition := p.tokens[definitionEnd].Position
		definition = strings.TrimSpace(string(p.source[startPosition:endPosition]))
	}
	return CreateView{Name: name, Definition: definition, Columns: columns, OrReplace: orReplace, AlterOnly: alterOnly, HasCreateOptions: hasCreateOptions}, nil
}

func (p *Parser) isTableConstraint() bool {
	return p.is("PRIMARY") || p.is("UNIQUE") || p.is("KEY") || p.is("INDEX") || p.is("CONSTRAINT") || p.is("FOREIGN") || p.is("CHECK")
}

func (p *Parser) skipDefinitionTail() {
	depth := 0
	for p.current().Kind != TokenEOF {
		switch p.current().Kind {
		case TokenLParen:
			depth++
		case TokenRParen:
			if depth == 0 {
				return
			}
			depth--
		case TokenComma:
			if depth == 0 {
				return
			}
		}
		p.position++
	}
}

func (p *Parser) parseDatabaseOptions() error {
	for p.current().Kind != TokenEOF && p.current().Kind != TokenSemicolon {
		p.accept("DEFAULT")
		switch {
		case p.accept("CHARACTER"):
			if err := p.expect("SET"); err != nil {
				return err
			}
		case p.accept("CHARSET"):
		case p.accept("COLLATE"):
		default:
			return p.errorf("unsupported CREATE DATABASE option %q", p.current().Text)
		}
		if p.current().Kind == TokenOperator && p.current().Text == "=" {
			p.position++
		}
		if p.current().Kind != TokenIdentifier && p.current().Kind != TokenString {
			return p.errorf("expected database option value")
		}
		p.position++
	}
	return nil
}

func (p *Parser) parseDrop() (Statement, error) {
	if p.accept("USER") {
		ifExists := p.accept("IF")
		if ifExists {
			if err := p.expect("EXISTS"); err != nil {
				return nil, err
			}
		}
		accounts, err := p.parseAccountList()
		return DropUser{Accounts: accounts, IfExists: ifExists}, err
	}
	if p.accept("DATABASE") || p.accept("SCHEMA") {
		ifExists := p.accept("IF")
		if ifExists {
			if err := p.expect("EXISTS"); err != nil {
				return nil, err
			}
		}
		name, err := p.identifier()
		return DropDatabase{Name: name, IfExists: ifExists}, err
	}
	if p.accept("INDEX") || p.accept("KEY") {
		name, err := p.identifier()
		if err != nil {
			return nil, err
		}
		if err := p.expect("ON"); err != nil {
			return nil, err
		}
		table, err := p.identifier()
		return DropIndex{Name: name, Table: table}, err
	}
	if p.accept("TABLE") {
		ifExists := p.accept("IF")
		if ifExists {
			if err := p.expect("EXISTS"); err != nil {
				return nil, err
			}
		}
		var names []string
		for {
			name, err := p.identifier()
			if err != nil {
				return nil, err
			}
			names = append(names, name)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
		return DropTable{Names: names, IfExists: ifExists}, nil
	}
	if p.accept("VIEW") {
		ifExists := p.accept("IF")
		if ifExists {
			if err := p.expect("EXISTS"); err != nil {
				return nil, err
			}
		}
		var names []string
		for {
			name, err := p.identifier()
			if err != nil {
				return nil, err
			}
			names = append(names, name)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
		p.accept("RESTRICT")
		p.accept("CASCADE")
		return DropView{Names: names, IfExists: ifExists}, nil
	}
	return nil, p.errorf("expected DATABASE, TABLE, VIEW, or USER")
}

func (p *Parser) parseInsert(replace bool) (Statement, error) {
	if replace {
		p.accept("LOW_PRIORITY")
		p.accept("DELAYED")
	}
	ignore := !replace && p.accept("IGNORE")
	p.accept("INTO")
	table, err := p.identifier()
	if err != nil {
		return nil, err
	}
	var columns []string
	if p.acceptKind(TokenLParen) {
		for {
			name, err := p.identifier()
			if err != nil {
				return nil, err
			}
			columns = append(columns, name)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
		if err := p.expectKind(TokenRParen, ")"); err != nil {
			return nil, err
		}
	}
	statement := Insert{Table: table, Columns: columns, Ignore: ignore, Replace: replace}
	if p.accept("SELECT") {
		query, err := p.parseUnionSelect()
		if err != nil {
			return nil, err
		}
		statement.Select = query
	} else if p.accept("SET") {
		if len(columns) > 0 {
			return nil, p.errorf("INSERT ... SET cannot include a column list")
		}
		for {
			column, err := p.identifier()
			if err != nil {
				return nil, err
			}
			if err := p.expectOperator("="); err != nil {
				return nil, err
			}
			value, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			statement.Columns = append(statement.Columns, column)
			statement.SetValues = append(statement.SetValues, value)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
	} else {
		if !p.accept("VALUES") && !p.accept("VALUE") {
			return nil, p.errorf("expected VALUES, VALUE, SET, or SELECT")
		}
		for {
			if err := p.expectKind(TokenLParen, "("); err != nil {
				return nil, err
			}
			var row []Literal
			for {
				value, err := p.literal()
				if err != nil {
					return nil, err
				}
				row = append(row, value)
				if !p.acceptKind(TokenComma) {
					break
				}
			}
			if err := p.expectKind(TokenRParen, ")"); err != nil {
				return nil, err
			}
			statement.Values = append(statement.Values, row)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
	}
	if !replace && p.accept("ON") {
		if err := p.expect("DUPLICATE"); err != nil {
			return nil, err
		}
		if err := p.expect("KEY"); err != nil {
			return nil, err
		}
		if err := p.expect("UPDATE"); err != nil {
			return nil, err
		}
		for {
			column, err := p.identifier()
			if err != nil {
				return nil, err
			}
			if err := p.expectOperator("="); err != nil {
				return nil, err
			}
			value, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			statement.OnDuplicate = append(statement.OnDuplicate, InsertAssignment{Column: column, Value: value})
			if !p.acceptKind(TokenComma) {
				break
			}
		}
	}
	return statement, nil
}

func (p *Parser) parseSelect() (Statement, error) {
	statement := Select{}
	for {
		switch {
		case p.accept("DISTINCT"), p.accept("DISTINCTROW"):
			statement.Distinct = true
		case p.accept("ALL"), p.accept("HIGH_PRIORITY"), p.accept("STRAIGHT_JOIN"),
			p.accept("SQL_SMALL_RESULT"), p.accept("SQL_BIG_RESULT"), p.accept("SQL_BUFFER_RESULT"),
			p.accept("SQL_NO_CACHE"), p.accept("SQL_CALC_FOUND_ROWS"):
		default:
			goto selectItems
		}
	}

selectItems:
	for {
		start := p.position
		depth := 0
		for {
			token := p.current()
			if token.Kind == TokenEOF {
				break
			}
			if depth == 0 && (token.Kind == TokenRParen || p.is("UNION") || p.is("WITH")) {
				break
			}
			if token.Kind == TokenLParen {
				depth++
			}
			if token.Kind == TokenRParen {
				depth--
			}
			if depth == 0 && (token.Kind == TokenComma || p.is("FROM")) {
				break
			}
			p.position++
		}
		if start == p.position {
			return nil, p.errorf("expected select expression")
		}
		tokens := p.tokens[start:p.position]
		alias := ""
		for i := len(tokens) - 2; i >= 0; i-- {
			if strings.EqualFold(tokens[i].Text, "AS") {
				alias = tokens[i+1].Text
				tokens = tokens[:i]
				break
			}
		}
		if alias == "" && len(tokens) >= 2 && tokens[len(tokens)-1].Kind == TokenIdentifier && tokens[len(tokens)-2].Kind != TokenDot && !strings.EqualFold(tokens[len(tokens)-1].Text, "END") {
			alias = tokens[len(tokens)-1].Text
			tokens = tokens[:len(tokens)-1]
		}
		statement.Items = append(statement.Items, SelectItem{Expression: joinTokens(tokens), Alias: alias})
		if p.acceptKind(TokenComma) {
			continue
		}
		break
	}
	if p.accept("FROM") {
		table, alias, subquery, err := p.parseTableSource()
		if err != nil {
			return nil, err
		}
		statement.Table, statement.TableAlias, statement.Subquery = table, alias, subquery
		statement.Joins, err = p.parseJoinClauses()
		if err != nil {
			return nil, err
		}
	}
	if p.accept("WHERE") {
		expression, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		statement.Where = expression
	}
	if p.accept("GROUP") {
		if err := p.expect("BY"); err != nil {
			return nil, err
		}
		for {
			start := p.position
			depth := 0
			for p.current().Kind != TokenEOF {
				t := p.current()
				if depth == 0 && (t.Kind == TokenComma || p.is("HAVING") || p.is("ORDER") || p.is("LIMIT") || t.Kind == TokenSemicolon || t.Kind == TokenRParen) {
					break
				}
				if t.Kind == TokenLParen {
					depth++
				}
				if t.Kind == TokenRParen {
					depth--
				}
				p.position++
			}
			if start == p.position {
				return nil, p.errorf("expected GROUP BY expression")
			}
			statement.GroupBy = append(statement.GroupBy, joinTokens(p.tokens[start:p.position]))
			if !p.acceptKind(TokenComma) {
				break
			}
		}
	}
	if p.accept("HAVING") {
		expression, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		statement.Having = expression
	}
	if p.accept("ORDER") {
		if err := p.expect("BY"); err != nil {
			return nil, err
		}
		for {
			start := p.position
			depth := 0
			for p.current().Kind != TokenEOF && p.current().Kind != TokenSemicolon {
				token := p.current()
				if token.Kind == TokenLParen {
					depth++
				} else if token.Kind == TokenRParen {
					depth--
				}
				if depth == 0 && (token.Kind == TokenComma || p.is("LIMIT")) {
					break
				}
				p.position++
			}
			if start == p.position {
				return nil, p.errorf("expected ORDER BY expression")
			}
			tokens := p.tokens[start:p.position]
			order := Order{}
			last := strings.ToUpper(tokens[len(tokens)-1].Text)
			if last == "ASC" || last == "DESC" {
				order.Desc = last == "DESC"
				tokens = tokens[:len(tokens)-1]
			}
			if len(tokens) == 0 {
				return nil, p.errorf("expected ORDER BY expression")
			}
			order.Column = joinTokens(tokens)
			statement.OrderBy = append(statement.OrderBy, order)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
	}
	if p.accept("LIMIT") {
		first, err := p.integer()
		if err != nil {
			return nil, err
		}
		statement.HasLimit = true
		statement.Limit = first
		if p.acceptKind(TokenComma) {
			second, err := p.integer()
			if err != nil {
				return nil, err
			}
			statement.Offset, statement.Limit = first, second
		} else if p.accept("OFFSET") {
			offset, err := p.integer()
			if err != nil {
				return nil, err
			}
			statement.Offset = offset
		}
	}
	if p.accept("FOR") {
		if err := p.expect("UPDATE"); err != nil {
			return nil, err
		}
		statement.Locking = true
		if !p.accept("NOWAIT") && p.accept("SKIP") {
			if err := p.expect("LOCKED"); err != nil {
				return nil, err
			}
		}
	} else if p.accept("LOCK") {
		if err := p.expect("IN"); err != nil {
			return nil, err
		}
		if err := p.expect("SHARE"); err != nil {
			return nil, err
		}
		if err := p.expect("MODE"); err != nil {
			return nil, err
		}
		statement.Locking = true
	}
	return statement, nil
}

func (p *Parser) isSelectClauseStart() bool {
	return p.is("WHERE") || p.is("GROUP") || p.is("HAVING") || p.is("ORDER") || p.is("LIMIT") ||
		p.is("JOIN") || p.is("LEFT") || p.is("RIGHT") || p.is("INNER") || p.is("CROSS") || p.is("FOR") || p.is("LOCK") || p.is("ON") || p.is("WITH") || p.is("UNION") ||
		p.is("SET") || p.is("USING") || p.is("FROM")
}

func (p *Parser) parseTableSource() (string, string, Query, error) {
	if p.acceptKind(TokenLParen) {
		if err := p.expect("SELECT"); err != nil {
			return "", "", nil, err
		}
		subquery, err := p.parseUnionSelect()
		if err != nil {
			return "", "", nil, err
		}
		if err := p.expectKind(TokenRParen, ")"); err != nil {
			return "", "", nil, err
		}
		p.accept("AS")
		alias, err := p.identifier()
		if err != nil {
			return "", "", nil, p.errorf("derived table requires an alias")
		}
		return "", alias, subquery, nil
	}
	table, err := p.identifier()
	if err != nil {
		return "", "", nil, err
	}
	alias := ""
	if p.accept("AS") {
		alias, err = p.identifier()
	} else if p.current().Kind == TokenIdentifier && !p.isSelectClauseStart() {
		alias, err = p.identifier()
	}
	return table, alias, nil, err
}

func (p *Parser) parseJoinClauses() ([]Join, error) {
	var joins []Join
	for {
		joinType := ""
		switch {
		case p.accept("LEFT"):
			joinType = "LEFT"
			p.accept("OUTER")
		case p.accept("RIGHT"):
			joinType = "RIGHT"
			p.accept("OUTER")
		case p.accept("INNER"):
			joinType = "INNER"
		case p.accept("CROSS"):
			joinType = "CROSS"
		case p.accept("JOIN"):
			joinType = "INNER"
		}
		if joinType == "" {
			return joins, nil
		}
		if !strings.EqualFold(p.tokens[p.position-1].Text, "JOIN") {
			if err := p.expect("JOIN"); err != nil {
				return nil, err
			}
		}
		table, alias, subquery, err := p.parseTableSource()
		if err != nil {
			return nil, err
		}
		join := Join{Type: joinType, Table: table, TableAlias: alias, Subquery: subquery}
		if joinType != "CROSS" {
			if err := p.expect("ON"); err != nil {
				return nil, err
			}
			join.On, err = p.parseExpr(0)
			if err != nil {
				return nil, err
			}
		}
		joins = append(joins, join)
	}
}

func (p *Parser) parseUpdate() (Statement, error) {
	table, alias, subquery, err := p.parseTableSource()
	if err != nil {
		return nil, err
	}
	if subquery != nil {
		return nil, p.errorf("UPDATE target must be a base table")
	}
	joins, err := p.parseJoinClauses()
	if err != nil {
		return nil, err
	}
	if err := p.expect("SET"); err != nil {
		return nil, err
	}
	assignments := make([]UpdateAssignment, 0, 1)
	for {
		column, err := p.identifier()
		if err != nil {
			return nil, err
		}
		if err := p.expectOperator("="); err != nil {
			return nil, err
		}
		value, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, UpdateAssignment{Column: column, Value: value})
		if !p.acceptKind(TokenComma) {
			break
		}
	}
	var where Expr
	if p.accept("WHERE") {
		where, err = p.parseExpr(0)
		if err != nil {
			return nil, err
		}
	}
	statement := Update{Table: table, TableAlias: alias, Joins: joins, Assignments: assignments, Where: where}
	if p.accept("LIMIT") {
		statement.Limit, err = p.integer()
		if err != nil {
			return nil, err
		}
		statement.HasLimit = true
	}
	return statement, nil
}

func (p *Parser) parseDelete() (Statement, error) {
	statement := Delete{}
	if p.accept("FROM") {
		start := p.position
		first, err := p.parseDeleteTarget()
		if err != nil {
			return nil, err
		}
		if p.is("USING") || p.current().Kind == TokenComma {
			statement.Targets = append(statement.Targets, first)
			for p.acceptKind(TokenComma) {
				target, targetErr := p.parseDeleteTarget()
				if targetErr != nil {
					return nil, targetErr
				}
				statement.Targets = append(statement.Targets, target)
			}
			if err := p.expect("USING"); err != nil {
				return nil, err
			}
		} else {
			p.position = start
		}
	} else {
		for {
			target, err := p.parseDeleteTarget()
			if err != nil {
				return nil, err
			}
			statement.Targets = append(statement.Targets, target)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
		if err := p.expect("FROM"); err != nil {
			return nil, err
		}
	}
	table, alias, subquery, err := p.parseTableSource()
	if err != nil {
		return nil, err
	}
	if subquery != nil {
		return nil, p.errorf("DELETE source must start with a base table")
	}
	statement.Table, statement.TableAlias = table, alias
	statement.Joins, err = p.parseJoinClauses()
	if err != nil {
		return nil, err
	}
	var where Expr
	if p.accept("WHERE") {
		where, err = p.parseExpr(0)
		if err != nil {
			return nil, err
		}
	}
	statement.Where = where
	if p.accept("LIMIT") {
		statement.Limit, err = p.integer()
		if err != nil {
			return nil, err
		}
		statement.HasLimit = true
	}
	return statement, nil
}

func (p *Parser) parseDeleteTarget() (string, error) {
	token := p.current()
	if token.Kind != TokenIdentifier {
		return "", p.errorf("expected delete target, got %q", token.Text)
	}
	p.position++
	target := token.Text
	if p.acceptKind(TokenDot) {
		if p.acceptKind(TokenStar) {
			return target, nil
		}
		if p.current().Kind != TokenIdentifier {
			return "", p.errorf("expected identifier or * after delete target dot")
		}
		target += "." + p.current().Text
		p.position++
		if p.acceptKind(TokenDot) && !p.acceptKind(TokenStar) {
			return "", p.errorf("expected * after delete target dot")
		}
	}
	return target, nil
}

func (p *Parser) parseShow() (Statement, error) {
	if p.accept("GRANTS") {
		statement := ShowGrants{}
		if p.accept("FOR") {
			account, err := p.accountName()
			if err != nil {
				return nil, err
			}
			statement.Account = account
			statement.ForAccount = true
		}
		return statement, nil
	}
	if p.accept("FULL") {
		if p.accept("TABLES") {
			return Show{What: "FULL TABLES"}, nil
		}
		if p.accept("COLUMNS") || p.accept("FIELDS") {
			return p.parseShowColumns(true)
		}
		return nil, p.errorf("expected TABLES, COLUMNS, or FIELDS after FULL")
	}
	if p.accept("DATABASES") || p.accept("SCHEMAS") {
		return Show{What: "DATABASES"}, nil
	}
	if p.accept("TABLES") {
		return Show{What: "TABLES"}, nil
	}
	if p.accept("INDEX") || p.accept("INDEXES") || p.accept("KEYS") {
		if !p.accept("FROM") && !p.accept("IN") {
			return nil, p.errorf("expected FROM or IN")
		}
		name, err := p.identifier()
		if err != nil {
			return nil, err
		}
		if p.accept("FROM") || p.accept("IN") {
			database, err := p.identifier()
			if err != nil {
				return nil, err
			}
			if !strings.Contains(name, ".") {
				name = database + "." + name
			}
		}
		return Show{What: "INDEX", Name: name}, nil
	}
	if p.accept("CREATE") {
		if p.accept("USER") {
			account, err := p.accountName()
			return ShowCreateUser{Account: account}, err
		}
		if p.accept("TABLE") {
			name, err := p.identifier()
			return Show{What: "CREATE TABLE", Name: name}, err
		}
		if p.accept("DATABASE") {
			what := "CREATE DATABASE"
			if p.accept("IF") {
				if err := p.expect("NOT"); err != nil {
					return nil, err
				}
				if err := p.expect("EXISTS"); err != nil {
					return nil, err
				}
				what = "CREATE DATABASE IF NOT EXISTS"
			}
			name, err := p.identifier()
			return Show{What: what, Name: name}, err
		}
		if p.accept("VIEW") {
			name, err := p.identifier()
			return Show{What: "CREATE VIEW", Name: name}, err
		}
	}
	if p.accept("COLUMNS") || p.accept("FIELDS") {
		return p.parseShowColumns(false)
	}
	return nil, p.errorf("unsupported SHOW command")
}

func (p *Parser) parseShowColumns(full bool) (Statement, error) {
	if !p.accept("FROM") && !p.accept("IN") {
		return nil, p.errorf("SHOW COLUMNS requires FROM or IN")
	}
	name, err := p.identifier()
	if err != nil {
		return nil, err
	}
	if p.accept("FROM") || p.accept("IN") {
		if strings.Contains(name, ".") {
			return nil, p.errorf("SHOW COLUMNS specifies a database twice")
		}
		database, databaseErr := p.identifier()
		if databaseErr != nil {
			return nil, databaseErr
		}
		name = database + "." + name
	}
	statement := Show{What: "COLUMNS", Name: name, Full: full}
	if p.accept("LIKE") {
		statement.Pattern, err = p.stringValue()
		return statement, err
	}
	if p.accept("WHERE") {
		statement.Where, err = p.parseExpr(0)
	}
	return statement, err
}

func (p *Parser) parseAlter() (Statement, error) {
	if p.is("VIEW") || p.is("ALGORITHM") || p.is("DEFINER") || p.is("SQL") {
		return p.parseCreateView(true, true)
	}
	if p.accept("TABLE") {
		table, err := p.identifier()
		if err != nil {
			return nil, err
		}
		actions := make([]Statement, 0, 1)
		for {
			action, actionErr := p.parseAlterTableAction(table)
			if actionErr != nil {
				return nil, actionErr
			}
			actions = append(actions, action)
			if !p.acceptKind(TokenComma) {
				break
			}
		}
		if len(actions) == 1 {
			return actions[0], nil
		}
		for _, action := range actions {
			if _, renamed := action.(RenameTable); renamed {
				return nil, p.errorf("RENAME cannot be combined with other ALTER TABLE operations")
			}
		}
		return AlterTableBatch{Table: table, Actions: actions}, nil
	}
	passwordForm := p.accept("PASSWORD")
	if passwordForm {
		p.accept("FOR")
		account, err := p.accountName()
		if err != nil {
			return nil, err
		}
		if p.current().Kind == TokenOperator && p.current().Text == "=" {
			p.position++
		} else {
			p.accept("TO")
			p.accept("BY")
		}
		password, err := p.stringValue()
		return SetPassword{Account: account, Password: password}, err
	}
	if err := p.expect("USER"); err != nil {
		return nil, err
	}
	ifExists := p.accept("IF")
	if ifExists {
		if err := p.expect("EXISTS"); err != nil {
			return nil, err
		}
	}
	users, err := p.parseUserSpecs(true)
	return AlterUser{Users: users, IfExists: ifExists}, err
}

func (p *Parser) parseAlterTableAction(table string) (Statement, error) {
	if p.accept("COMMENT") {
		if p.current().Kind == TokenOperator && p.current().Text == "=" {
			p.position++
		}
		if p.current().Kind != TokenString {
			return nil, p.errorf("expected table comment")
		}
		comment := p.current().Text
		p.position++
		return AlterTableComment{Table: table, Comment: comment}, nil
	}
	if p.accept("ADD") {
		if p.accept("PRIMARY") {
			if err := p.expect("KEY"); err != nil {
				return nil, err
			}
			columns, err := p.parseIndexColumns()
			if err != nil {
				return nil, err
			}
			p.parseIndexMethod()
			return CreateIndex{Name: "PRIMARY", Table: table, Columns: columns, Unique: true, Primary: true}, nil
		}
		constraintName := ""
		if p.accept("CONSTRAINT") {
			var err error
			constraintName, err = p.identifier()
			if err != nil {
				return nil, err
			}
		}
		if p.is("FOREIGN") {
			foreignKey, foreignKeyErr := p.parseForeignKeyDefinition(constraintName)
			if foreignKeyErr != nil {
				return nil, foreignKeyErr
			}
			return AlterForeignKey{Table: table, ForeignKey: foreignKey}, nil
		}
		if p.accept("CHECK") {
			expression, checkErr := p.parseParenthesizedDefinition()
			if checkErr != nil {
				return nil, checkErr
			}
			return AlterCheck{Table: table, Check: CheckDef{Name: constraintName, Expression: expression}}, nil
		}
		unique := p.accept("UNIQUE")
		if !unique && !p.is("INDEX") && !p.is("KEY") {
			if constraintName != "" {
				return nil, p.errorf("expected FOREIGN KEY, CHECK, or UNIQUE after CONSTRAINT")
			}
			p.accept("COLUMN")
			ifNotExists := false
			if p.accept("IF") {
				if err := p.expect("NOT"); err != nil {
					return nil, err
				}
				if err := p.expect("EXISTS"); err != nil {
					return nil, err
				}
				ifNotExists = true
			}
			column, columnErr := p.parseColumnDefinition()
			if columnErr != nil {
				return nil, columnErr
			}
			addition := AddColumn{Table: table, Column: column, IfNotExists: ifNotExists}
			if p.accept("FIRST") {
				addition.First = true
			} else if p.accept("AFTER") {
				addition.After, columnErr = p.identifier()
				if columnErr != nil {
					return nil, columnErr
				}
			}
			return addition, nil
		}
		p.accept("INDEX")
		p.accept("KEY")
		name := constraintName
		if p.current().Kind != TokenLParen && !p.is("USING") {
			parsedName, err := p.identifier()
			if err != nil {
				return nil, err
			}
			name = parsedName
		}
		p.parseIndexMethod()
		columns, err := p.parseIndexColumns()
		if err != nil {
			return nil, err
		}
		p.parseIndexMethod()
		if name == "" {
			name = columns[0]
		}
		return CreateIndex{Name: name, Table: table, Columns: columns, Unique: unique}, nil
	}
	if p.accept("DROP") {
		if p.accept("PRIMARY") {
			if err := p.expect("KEY"); err != nil {
				return nil, err
			}
			return DropIndex{Name: "PRIMARY", Table: table}, nil
		}
		if p.accept("FOREIGN") {
			if err := p.expect("KEY"); err != nil {
				return nil, err
			}
			name, nameErr := p.identifier()
			return AlterForeignKey{Table: table, Drop: true, Name: name}, nameErr
		}
		if p.accept("CHECK") || p.accept("CONSTRAINT") {
			name, nameErr := p.identifier()
			return AlterCheck{Table: table, Drop: true, Name: name}, nameErr
		}
		if p.accept("COLUMN") || !p.is("INDEX") && !p.is("KEY") {
			ifExists := false
			if p.accept("IF") {
				if err := p.expect("EXISTS"); err != nil {
					return nil, err
				}
				ifExists = true
			}
			name, nameErr := p.identifier()
			return DropColumn{Table: table, Name: name, IfExists: ifExists}, nameErr
		}
		if !p.accept("INDEX") && !p.accept("KEY") {
			return nil, p.errorf("expected INDEX or KEY")
		}
		name, err := p.identifier()
		return DropIndex{Name: name, Table: table}, err
	}
	if p.accept("MODIFY") {
		p.accept("COLUMN")
		column, err := p.parseColumnDefinition()
		if err != nil {
			return nil, err
		}
		p.skipDefinitionTail()
		return AlterColumn{Table: table, OldName: column.Name, Column: column}, nil
	}
	if p.accept("CHANGE") {
		p.accept("COLUMN")
		oldName, err := p.identifier()
		if err != nil {
			return nil, err
		}
		column, err := p.parseColumnDefinition()
		if err != nil {
			return nil, err
		}
		p.skipDefinitionTail()
		return AlterColumn{Table: table, OldName: oldName, Column: column}, nil
	}
	if p.accept("ALTER") {
		p.accept("COLUMN")
		name, err := p.identifier()
		if err != nil {
			return nil, err
		}
		if p.accept("DROP") {
			if err := p.expect("DEFAULT"); err != nil {
				return nil, err
			}
			return AlterColumnDefault{Table: table, Name: name, Drop: true}, nil
		}
		if err := p.expect("SET"); err != nil {
			return nil, err
		}
		if err := p.expect("DEFAULT"); err != nil {
			return nil, err
		}
		literal, expression, err := p.parseDefaultValue()
		return AlterColumnDefault{Table: table, Name: name, Default: literal, DefaultExpression: expression}, err
	}
	if p.accept("RENAME") {
		if p.accept("INDEX") || p.accept("KEY") {
			oldName, err := p.identifier()
			if err != nil {
				return nil, err
			}
			if err := p.expect("TO"); err != nil {
				return nil, err
			}
			newName, err := p.identifier()
			return RenameIndex{Table: table, OldName: oldName, NewName: newName}, err
		}
		if p.accept("COLUMN") {
			oldName, err := p.identifier()
			if err != nil {
				return nil, err
			}
			if err := p.expect("TO"); err != nil {
				return nil, err
			}
			newName, err := p.identifier()
			return RenameColumn{Table: table, OldName: oldName, NewName: newName}, err
		}
		p.accept("TO")
		p.accept("AS")
		name, nameErr := p.identifier()
		return RenameTable{Pairs: []RenameTablePair{{From: table, To: name}}}, nameErr
	}
	return nil, p.errorf("unsupported ALTER TABLE operation")
}

func (p *Parser) parseColumnDefinition() (ColumnDef, error) {
	columnName, err := p.identifier()
	if err != nil {
		return ColumnDef{}, err
	}
	columnType, err := p.identifier()
	if err != nil {
		return ColumnDef{}, err
	}
	sqlType := strings.ToLower(columnType)
	if (strings.EqualFold(columnType, "DOUBLE") && p.accept("PRECISION")) ||
		(strings.EqualFold(columnType, "NATIONAL") && (p.accept("CHAR") || p.accept("VARCHAR"))) {
		sqlType += " " + strings.ToLower(p.tokens[p.position-1].Text)
	}
	length := 0
	if p.acceptKind(TokenLParen) {
		arguments, firstLength, err := p.parseTypeArguments()
		if err != nil {
			return ColumnDef{}, err
		}
		length = firstLength
		sqlType += "(" + arguments + ")"
	}
	for p.is("UNSIGNED") || p.is("SIGNED") || p.is("ZEROFILL") {
		sqlType += " " + strings.ToLower(p.current().Text)
		p.position++
	}
	column := ColumnDef{Name: columnName, Type: columnType, SQLType: sqlType, Length: length, Nullable: true}
	for p.current().Kind != TokenEOF && p.current().Kind != TokenComma && p.current().Kind != TokenRParen {
		switch {
		case p.accept("NOT"):
			if err := p.expect("NULL"); err != nil {
				return ColumnDef{}, err
			}
			column.Nullable, column.NullabilitySpecified = false, true
		case p.accept("NULL"):
			column.Nullable, column.NullabilitySpecified = true, true
		case p.accept("DEFAULT"):
			literal, expression, err := p.parseDefaultValue()
			if err != nil {
				return ColumnDef{}, err
			}
			column.HasDefault, column.Default, column.DefaultExpression = true, literal, expression
		case p.accept("AUTO_INCREMENT"):
			column.AutoIncrement = true
			column.Nullable, column.NullabilitySpecified = false, true
		case p.accept("COMMENT"):
			if p.current().Kind != TokenString {
				return ColumnDef{}, p.errorf("expected column comment")
			}
			column.Comment = p.current().Text
			p.position++
		case p.accept("ON"):
			if err := p.expect("UPDATE"); err != nil {
				return ColumnDef{}, err
			}
			if p.current().Kind != TokenIdentifier {
				return ColumnDef{}, p.errorf("expected ON UPDATE expression")
			}
			column.OnUpdate = strings.ToLower(p.current().Text)
			p.position++
			if p.acceptKind(TokenLParen) {
				if err := p.expectKind(TokenRParen, ")"); err != nil {
					return ColumnDef{}, err
				}
				column.OnUpdate += "()"
			}
		case p.accept("PRIMARY"):
			if err := p.expect("KEY"); err != nil {
				return ColumnDef{}, err
			}
			column.PrimaryKey = true
			column.Nullable, column.NullabilitySpecified = false, true
		case p.accept("UNIQUE"):
			p.accept("KEY")
			column.Unique = true
		case p.accept("CHECK"):
			if err := p.expectKind(TokenLParen, "("); err != nil {
				return ColumnDef{}, err
			}
			start := p.position
			depth := 1
			for p.current().Kind != TokenEOF && depth > 0 {
				if p.current().Kind == TokenLParen {
					depth++
				}
				if p.current().Kind == TokenRParen {
					depth--
				}
				if depth > 0 {
					p.position++
				}
			}
			column.Check = joinTokens(p.tokens[start:p.position])
			if err := p.expectKind(TokenRParen, ")"); err != nil {
				return ColumnDef{}, err
			}
		case p.accept("CHARACTER"):
			if err := p.expect("SET"); err != nil {
				return ColumnDef{}, err
			}
			if _, err := p.identifier(); err != nil {
				return ColumnDef{}, err
			}
		case p.accept("COLLATE"):
			if _, err := p.identifier(); err != nil {
				return ColumnDef{}, err
			}
		default:
			// Positioning, generated-column and other non-semantic compatibility
			// clauses remain accepted by the caller's definition-tail skipper.
			return column, nil
		}
	}
	return column, nil
}

func (p *Parser) parseDefaultValue() (Literal, string, error) {
	if p.current().Kind == TokenIdentifier && !p.is("NULL") && !p.is("TRUE") && !p.is("FALSE") {
		expression := strings.ToLower(p.current().Text)
		p.position++
		if p.acceptKind(TokenLParen) {
			if err := p.expectKind(TokenRParen, ")"); err != nil {
				return Literal{}, "", err
			}
			expression += "()"
		}
		return Literal{}, expression, nil
	}
	literal, err := p.literal()
	return literal, "", err
}

func (p *Parser) parseTypeArguments() (string, int, error) {
	var result strings.Builder
	firstLength := 0
	first := true
	for p.current().Kind != TokenEOF && p.current().Kind != TokenRParen {
		token := p.current()
		switch token.Kind {
		case TokenComma:
			result.WriteByte(',')
		case TokenString:
			result.WriteByte('\'')
			result.WriteString(strings.ReplaceAll(token.Text, "'", "''"))
			result.WriteByte('\'')
		case TokenNumber, TokenIdentifier, TokenOperator:
			result.WriteString(token.Text)
		default:
			return "", 0, p.errorf("unsupported type argument %q", token.Text)
		}
		if first && token.Kind == TokenNumber {
			if parsed, err := strconv.Atoi(token.Text); err == nil && parsed >= 0 {
				firstLength = parsed
			}
		}
		if token.Kind != TokenComma {
			first = false
		}
		p.position++
	}
	if err := p.expectKind(TokenRParen, ")"); err != nil {
		return "", 0, err
	}
	if result.Len() == 0 {
		return "", 0, p.errorf("type arguments cannot be empty")
	}
	return result.String(), firstLength, nil
}

func (p *Parser) parseIndexColumns() ([]string, error) {
	if err := p.expectKind(TokenLParen, "("); err != nil {
		return nil, err
	}
	var columns []string
	for {
		column, err := p.identifier()
		if err != nil {
			return nil, err
		}
		columns = append(columns, column)
		if p.acceptKind(TokenLParen) {
			if _, err := p.expectKindValue(TokenNumber, "index prefix length"); err != nil {
				return nil, err
			}
			if err := p.expectKind(TokenRParen, ")"); err != nil {
				return nil, err
			}
		}
		p.accept("ASC")
		p.accept("DESC")
		if !p.acceptKind(TokenComma) {
			break
		}
	}
	if err := p.expectKind(TokenRParen, ")"); err != nil {
		return nil, err
	}
	return columns, nil
}

func (p *Parser) parseTableIndexDefinition(unique bool) (IndexDef, error) {
	name := ""
	if p.current().Kind != TokenLParen && !p.is("USING") {
		var err error
		name, err = p.identifier()
		if err != nil {
			return IndexDef{}, err
		}
	}
	p.parseIndexMethod()
	columns, err := p.parseIndexColumns()
	if err != nil {
		return IndexDef{}, err
	}
	p.parseIndexMethod()
	if name == "" {
		name = columns[0]
	}
	return IndexDef{Name: name, Columns: columns, Unique: unique}, nil
}

func (p *Parser) parseIndexMethod() {
	if p.accept("USING") && p.current().Kind == TokenIdentifier {
		p.position++
	}
}

func (p *Parser) parseParenthesizedDefinition() (string, error) {
	if err := p.expectKind(TokenLParen, "("); err != nil {
		return "", err
	}
	start := p.position
	depth := 1
	for p.current().Kind != TokenEOF && depth > 0 {
		if p.current().Kind == TokenLParen {
			depth++
		}
		if p.current().Kind == TokenRParen {
			depth--
		}
		if depth > 0 {
			p.position++
		}
	}
	if depth != 0 {
		return "", p.errorf("unterminated parenthesized definition")
	}
	expression := joinTokens(p.tokens[start:p.position])
	if err := p.expectKind(TokenRParen, ")"); err != nil {
		return "", err
	}
	return expression, nil
}

func (p *Parser) parseForeignKeyDefinition(name string) (ForeignKeyDef, error) {
	if err := p.expect("FOREIGN"); err != nil {
		return ForeignKeyDef{}, err
	}
	if err := p.expect("KEY"); err != nil {
		return ForeignKeyDef{}, err
	}
	if p.current().Kind == TokenIdentifier && p.tokens[p.position+1].Kind == TokenLParen {
		if name == "" {
			name = p.current().Text
		}
		p.position++
	}
	local, err := p.parseIndexColumns()
	if err != nil {
		return ForeignKeyDef{}, err
	}
	if err := p.expect("REFERENCES"); err != nil {
		return ForeignKeyDef{}, err
	}
	refTable, err := p.identifier()
	if err != nil {
		return ForeignKeyDef{}, err
	}
	refColumns, err := p.parseIndexColumns()
	if err != nil {
		return ForeignKeyDef{}, err
	}
	definition := ForeignKeyDef{Name: name, Columns: local, RefTable: refTable, RefColumns: refColumns}
	for p.current().Kind != TokenEOF && p.current().Kind != TokenComma && p.current().Kind != TokenRParen {
		if p.accept("ON") {
			actionType := ""
			if p.accept("DELETE") {
				actionType = "DELETE"
			} else if p.accept("UPDATE") {
				actionType = "UPDATE"
			} else {
				return ForeignKeyDef{}, p.errorf("expected DELETE or UPDATE after ON")
			}
			action := ""
			switch {
			case p.accept("RESTRICT"):
				action = "RESTRICT"
			case p.accept("CASCADE"):
				action = "CASCADE"
			case p.accept("NO"):
				if err := p.expect("ACTION"); err != nil {
					return ForeignKeyDef{}, err
				}
				action = "NO ACTION"
			case p.accept("SET"):
				if err := p.expect("NULL"); err != nil {
					return ForeignKeyDef{}, err
				}
				action = "SET NULL"
			default:
				return ForeignKeyDef{}, p.errorf("unsupported referential action %q", p.current().Text)
			}
			if actionType == "DELETE" {
				definition.OnDelete = action
			} else {
				definition.OnUpdate = action
			}
			continue
		}
		if p.accept("MATCH") {
			if p.current().Kind == TokenIdentifier {
				p.position++
			}
			continue
		}
		return ForeignKeyDef{}, p.errorf("unsupported foreign key option %q", p.current().Text)
	}
	return definition, nil
}

func (p *Parser) parseRename() (Statement, error) {
	if p.accept("TABLE") {
		statement := RenameTable{}
		for {
			from, err := p.identifier()
			if err != nil {
				return nil, err
			}
			if err := p.expect("TO"); err != nil {
				return nil, err
			}
			to, err := p.identifier()
			if err != nil {
				return nil, err
			}
			statement.Pairs = append(statement.Pairs, RenameTablePair{From: from, To: to})
			if !p.acceptKind(TokenComma) {
				return statement, nil
			}
		}
	}
	if err := p.expect("USER"); err != nil {
		return nil, err
	}
	statement := RenameUser{}
	for {
		from, err := p.accountName()
		if err != nil {
			return nil, err
		}
		if err := p.expect("TO"); err != nil {
			return nil, err
		}
		to, err := p.accountName()
		if err != nil {
			return nil, err
		}
		statement.Pairs = append(statement.Pairs, RenameUserPair{From: from, To: to})
		if !p.acceptKind(TokenComma) {
			return statement, nil
		}
	}
}

func (p *Parser) parseSet() (Statement, error) {
	if err := p.expect("PASSWORD"); err != nil {
		return nil, err
	}
	statement := SetPassword{}
	if p.accept("FOR") {
		account, err := p.accountName()
		if err != nil {
			return nil, err
		}
		statement.Account = account
	}
	if p.current().Kind == TokenOperator && p.current().Text == "=" {
		p.position++
	} else {
		p.accept("TO")
	}
	password, err := p.stringValue()
	statement.Password = password
	return statement, err
}

func (p *Parser) parseGrant() (Statement, error) {
	privileges, err := p.parsePrivilegeList("ON")
	if err != nil {
		return nil, err
	}
	if err := p.expect("ON"); err != nil {
		return nil, err
	}
	p.accept("TABLE")
	database, table, err := p.parsePrivilegeScope()
	if err != nil {
		return nil, err
	}
	if err := p.expect("TO"); err != nil {
		return nil, err
	}
	accounts, err := p.parseAccountList()
	if err != nil {
		return nil, err
	}
	grantOption := false
	if p.accept("WITH") {
		if err := p.expect("GRANT"); err != nil {
			return nil, err
		}
		if err := p.expect("OPTION"); err != nil {
			return nil, err
		}
		grantOption = true
	}
	return Grant{Privileges: privileges, Database: database, Table: table, Accounts: accounts, GrantOption: grantOption}, nil
}

func (p *Parser) parseRevoke() (Statement, error) {
	grantOptionOnly := false
	if p.accept("GRANT") {
		if err := p.expect("OPTION"); err != nil {
			return nil, err
		}
		if err := p.expect("FOR"); err != nil {
			return nil, err
		}
		grantOptionOnly = true
	}
	privileges, err := p.parsePrivilegeList("ON")
	if err != nil {
		return nil, err
	}
	if err := p.expect("ON"); err != nil {
		return nil, err
	}
	p.accept("TABLE")
	database, table, err := p.parsePrivilegeScope()
	if err != nil {
		return nil, err
	}
	if err := p.expect("FROM"); err != nil {
		return nil, err
	}
	accounts, err := p.parseAccountList()
	if err != nil {
		return nil, err
	}
	return Revoke{Privileges: privileges, Database: database, Table: table, Accounts: accounts, GrantOptionOnly: grantOptionOnly}, nil
}

func (p *Parser) parsePrivilegeList(stop string) ([]string, error) {
	var privileges []string
	var words []string
	for !p.is(stop) && p.current().Kind != TokenEOF {
		if p.acceptKind(TokenComma) {
			if len(words) == 0 {
				return nil, p.errorf("empty privilege")
			}
			privileges = append(privileges, strings.Join(words, " "))
			words = nil
			continue
		}
		if p.current().Kind != TokenIdentifier {
			return nil, p.errorf("unsupported privilege syntax near %q", p.current().Text)
		}
		words = append(words, p.current().Text)
		p.position++
	}
	if len(words) > 0 {
		privileges = append(privileges, strings.Join(words, " "))
	}
	if len(privileges) == 0 {
		return nil, p.errorf("GRANT/REVOKE requires at least one privilege")
	}
	return privileges, nil
}

func (p *Parser) parsePrivilegeScope() (string, string, error) {
	part := func() (string, error) {
		if p.current().Kind == TokenStar {
			p.position++
			return "*", nil
		}
		if p.current().Kind != TokenIdentifier {
			return "", p.errorf("expected privilege scope")
		}
		value := p.current().Text
		p.position++
		return value, nil
	}
	database, err := part()
	if err != nil {
		return "", "", err
	}
	if err := p.expectKind(TokenDot, "."); err != nil {
		return "", "", err
	}
	table, err := part()
	return database, table, err
}

func (p *Parser) parseIfNotExists() (bool, error) {
	if !p.accept("IF") {
		return false, nil
	}
	if err := p.expect("NOT"); err != nil {
		return false, err
	}
	if err := p.expect("EXISTS"); err != nil {
		return false, err
	}
	return true, nil
}

func (p *Parser) parseUserSpecs(requirePassword bool) ([]UserSpec, error) {
	var users []UserSpec
	for {
		account, err := p.accountName()
		if err != nil {
			return nil, err
		}
		spec := UserSpec{Account: account}
		if p.accept("IDENTIFIED") {
			if p.accept("WITH") {
				if p.current().Kind != TokenIdentifier {
					return nil, p.errorf("expected authentication plugin")
				}
				p.position++
			}
			if err := p.expect("BY"); err != nil {
				return nil, err
			}
			password, err := p.stringValue()
			if err != nil {
				return nil, err
			}
			spec.Password = password
		} else if requirePassword {
			return nil, p.errorf("ALTER USER requires IDENTIFIED BY")
		}
		users = append(users, spec)
		if !p.acceptKind(TokenComma) {
			return users, nil
		}
	}
}

func (p *Parser) parseAccountList() ([]Account, error) {
	var accounts []Account
	for {
		account, err := p.accountName()
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
		if !p.acceptKind(TokenComma) {
			return accounts, nil
		}
	}
}

func (p *Parser) accountName() (Account, error) {
	var account Account
	if p.current().Kind != TokenString && p.current().Kind != TokenIdentifier {
		return account, p.errorf("expected account name")
	}
	account.Username = p.current().Text
	p.position++
	account.Host = "%"
	if p.acceptKind(TokenAt) {
		if p.current().Kind != TokenString && p.current().Kind != TokenIdentifier {
			return account, p.errorf("expected account host")
		}
		account.Host = p.current().Text
		p.position++
	}
	return account, nil
}

func (p *Parser) parseExport() (Statement, error) {
	if err := p.expect("DATABASE"); err != nil {
		return nil, err
	}
	name, err := p.identifier()
	if err != nil {
		return nil, err
	}
	path := "backup.sql"
	if p.accept("TO") {
		path, err = p.stringValue()
	}
	return ExportDatabase{Name: name, Path: path}, err
}

func (p *Parser) parseExpr(min int) (Expr, error) {
	left, err := p.operand()
	if err != nil {
		return nil, err
	}
	for {
		operator := strings.ToUpper(p.current().Text)
		if min <= 3 && operator == "IS" {
			p.position++
			not := p.accept("NOT")
			if !p.is("NULL") && !p.is("TRUE") && !p.is("FALSE") {
				return nil, p.errorf("expected NULL, TRUE, or FALSE after IS")
			}
			target, targetErr := p.operand()
			if targetErr != nil {
				return nil, targetErr
			}
			left = IsExpr{Value: left, Target: target, Not: not}
			continue
		}
		not := false
		if min <= 3 && operator == "NOT" && p.position+1 < len(p.tokens) {
			next := strings.ToUpper(p.tokens[p.position+1].Text)
			if next == "LIKE" || next == "IN" || next == "BETWEEN" {
				not = true
				p.position++
				operator = next
			}
		}
		if min <= 3 && operator == "IN" {
			p.position++
			if err := p.expectKind(TokenLParen, "("); err != nil {
				return nil, err
			}
			if p.accept("SELECT") {
				query, queryErr := p.parseUnionSelect()
				if queryErr != nil {
					return nil, queryErr
				}
				if err := p.expectKind(TokenRParen, ")"); err != nil {
					return nil, err
				}
				left = InExpr{Value: left, Subquery: query, Not: not}
				continue
			}
			values := make([]Expr, 0)
			for {
				value, valueErr := p.parseExpr(0)
				if valueErr != nil {
					return nil, valueErr
				}
				values = append(values, value)
				if !p.acceptKind(TokenComma) {
					break
				}
			}
			if err := p.expectKind(TokenRParen, ")"); err != nil {
				return nil, err
			}
			left = InExpr{Value: left, Values: values, Not: not}
			continue
		}
		if min <= 3 && operator == "BETWEEN" {
			p.position++
			lower, lowerErr := p.parseExpr(4)
			if lowerErr != nil {
				return nil, lowerErr
			}
			if err := p.expect("AND"); err != nil {
				return nil, err
			}
			upper, upperErr := p.parseExpr(4)
			if upperErr != nil {
				return nil, upperErr
			}
			left = BetweenExpr{Value: left, Lower: lower, Upper: upper, Not: not}
			continue
		}
		precedence := map[string]int{"OR": 1, "AND": 2, "=": 3, "<=>": 3, "!=": 3, "<>": 3, ">": 3, "<": 3, ">=": 3, "<=": 3, "LIKE": 3, "+": 4, "-": 4, "*": 5, "/": 5, "%": 5}[operator]
		if precedence == 0 || precedence < min {
			break
		}
		p.position++
		right, err := p.parseExpr(precedence + 1)
		if err != nil {
			return nil, err
		}
		if not {
			operator = "NOT " + operator
		}
		left = BinaryExpr{Left: left, Operator: operator, Right: right}
	}
	return left, nil
}
func (p *Parser) operand() (Expr, error) {
	if p.accept("CASE") {
		result := CaseExpr{}
		if !p.is("WHEN") {
			var err error
			result.Operand, err = p.parseExpr(0)
			if err != nil {
				return nil, err
			}
		}
		for p.accept("WHEN") {
			when, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			if err := p.expect("THEN"); err != nil {
				return nil, err
			}
			then, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			result.Whens = append(result.Whens, CaseWhen{When: when, Then: then})
		}
		if p.accept("ELSE") {
			var err error
			result.Else, err = p.parseExpr(0)
			if err != nil {
				return nil, err
			}
		}
		if err := p.expect("END"); err != nil {
			return nil, err
		}
		return result, nil
	}
	if p.current().Kind == TokenLParen && p.position+1 < len(p.tokens) && strings.EqualFold(p.tokens[p.position+1].Text, "SELECT") {
		p.position += 2
		query, err := p.parseUnionSelect()
		if err != nil {
			return nil, err
		}
		if err := p.expectKind(TokenRParen, ")"); err != nil {
			return nil, err
		}
		return ScalarSubquery{Query: query}, nil
	}
	if p.accept("EXISTS") {
		if err := p.expectKind(TokenLParen, "("); err != nil {
			return nil, err
		}
		if err := p.expect("SELECT"); err != nil {
			return nil, err
		}
		query, err := p.parseUnionSelect()
		if err != nil {
			return nil, err
		}
		if err := p.expectKind(TokenRParen, ")"); err != nil {
			return nil, err
		}
		return ExistsExpr{Query: query}, nil
	}
	if p.accept("NOT") {
		// SQL gives unary NOT lower precedence than comparison predicates, but
		// higher precedence than AND/OR: NOT name LIKE 'x%' negates the LIKE.
		value, err := p.parseExpr(3)
		return UnaryExpr{Operator: "NOT", Value: value}, err
	}
	if p.acceptKind(TokenLParen) {
		expr, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		if p.acceptKind(TokenComma) {
			row := RowExpr{Values: []Expr{expr}}
			for {
				item, itemErr := p.parseExpr(0)
				if itemErr != nil {
					return nil, itemErr
				}
				row.Values = append(row.Values, item)
				if !p.acceptKind(TokenComma) {
					break
				}
			}
			if err := p.expectKind(TokenRParen, ")"); err != nil {
				return nil, err
			}
			return row, nil
		}
		if err := p.expectKind(TokenRParen, ")"); err != nil {
			return nil, err
		}
		return expr, nil
	}
	if p.accept("INTERVAL") {
		value, err := p.operand()
		if err != nil {
			return nil, err
		}
		unit, err := p.identifier()
		if err != nil {
			return nil, p.errorf("INTERVAL requires a unit")
		}
		return IntervalExpr{Value: value, Unit: unit}, nil
	}
	if p.is("CURRENT_TIMESTAMP") && p.position+1 < len(p.tokens) && p.tokens[p.position+1].Kind != TokenLParen {
		p.position++
		return FunctionExpr{Name: "CURRENT_TIMESTAMP"}, nil
	}
	if p.current().Kind == TokenIdentifier && p.position+1 < len(p.tokens) && p.tokens[p.position+1].Kind == TokenLParen {
		name := p.current().Text
		p.position += 2
		function := FunctionExpr{Name: name}
		if p.current().Kind == TokenStar {
			function.Star = true
			p.position++
		} else if p.current().Kind != TokenRParen {
			for {
				argument, err := p.parseExpr(0)
				if err != nil {
					return nil, err
				}
				function.Args = append(function.Args, argument)
				if !p.acceptKind(TokenComma) {
					break
				}
			}
		}
		if err := p.expectKind(TokenRParen, ")"); err != nil {
			return nil, err
		}
		if p.accept("OVER") {
			window := WindowExpr{Function: function}
			if err := p.expectKind(TokenLParen, "("); err != nil {
				return nil, err
			}
			if p.accept("PARTITION") {
				if err := p.expect("BY"); err != nil {
					return nil, err
				}
				for {
					expression, err := p.parseExpr(0)
					if err != nil {
						return nil, err
					}
					window.PartitionBy = append(window.PartitionBy, expression)
					if !p.acceptKind(TokenComma) {
						break
					}
				}
			}
			if p.accept("ORDER") {
				if err := p.expect("BY"); err != nil {
					return nil, err
				}
				for {
					expression, err := p.parseExpr(0)
					if err != nil {
						return nil, err
					}
					order := WindowOrder{Expression: expression}
					if p.accept("DESC") {
						order.Desc = true
					} else {
						p.accept("ASC")
					}
					window.OrderBy = append(window.OrderBy, order)
					if !p.acceptKind(TokenComma) {
						break
					}
				}
			}
			if err := p.expectKind(TokenRParen, ")"); err != nil {
				return nil, err
			}
			return window, nil
		}
		return function, nil
	}
	if p.current().Kind == TokenIdentifier && !p.is("NULL") && !p.is("TRUE") && !p.is("FALSE") {
		name, err := p.identifier()
		return Identifier{Name: name}, err
	}
	literal, err := p.literal()
	return LiteralExpr{Value: literal}, err
}
func (p *Parser) literal() (Literal, error) {
	token := p.current()
	switch {
	case token.Kind == TokenString:
		p.position++
		return Literal{Kind: LiteralString, Text: token.Text}, nil
	case token.Kind == TokenNumber:
		p.position++
		return Literal{Kind: LiteralNumber, Text: token.Text}, nil
	case p.accept("NULL"):
		return Literal{Kind: LiteralNull}, nil
	case p.accept("TRUE"):
		return Literal{Kind: LiteralBoolean, Text: "true"}, nil
	case p.accept("FALSE"):
		return Literal{Kind: LiteralBoolean, Text: "false"}, nil
	default:
		return Literal{}, p.errorf("expected literal, got %q", token.Text)
	}
}

func (p *Parser) identifier() (string, error) {
	token := p.current()
	if token.Kind != TokenIdentifier {
		return "", p.errorf("expected identifier, got %q", token.Text)
	}
	p.position++
	name := token.Text
	if p.acceptKind(TokenDot) {
		next := p.current()
		if next.Kind != TokenIdentifier {
			return "", p.errorf("expected identifier after dot")
		}
		p.position++
		name += "." + next.Text
	}
	return name, nil
}
func (p *Parser) userName() (string, error) {
	var name string
	var err error
	if p.current().Kind == TokenString {
		name = p.current().Text
		p.position++
	} else {
		name, err = p.identifier()
	}
	if err != nil {
		return "", err
	}
	if p.acceptKind(TokenAt) {
		if p.current().Kind == TokenString || p.current().Kind == TokenIdentifier {
			p.position++
		}
	}
	return name, nil
}
func (p *Parser) stringValue() (string, error) {
	token := p.current()
	if token.Kind != TokenString {
		return "", p.errorf("expected string")
	}
	p.position++
	return token.Text, nil
}
func (p *Parser) integer() (int, error) {
	token, err := p.expectKindValue(TokenNumber, "integer")
	if err != nil {
		return 0, err
	}
	result, err := strconv.Atoi(token.Text)
	if err != nil || result < 0 {
		return 0, p.errorf("invalid non-negative integer")
	}
	return result, nil
}
func (p *Parser) current() Token { return p.tokens[p.position] }
func (p *Parser) is(word string) bool {
	return p.current().Kind == TokenIdentifier && strings.EqualFold(p.current().Text, word)
}
func (p *Parser) accept(word string) bool {
	if p.is(word) {
		p.position++
		return true
	}
	return false
}
func (p *Parser) expect(word string) error {
	if !p.accept(word) {
		return p.errorf("expected %s", word)
	}
	return nil
}
func (p *Parser) acceptKind(kind TokenKind) bool {
	if p.current().Kind == kind {
		p.position++
		return true
	}
	return false
}
func (p *Parser) expectKind(kind TokenKind, name string) error {
	_, err := p.expectKindValue(kind, name)
	return err
}
func (p *Parser) expectKindValue(kind TokenKind, name string) (Token, error) {
	token := p.current()
	if token.Kind != kind {
		return Token{}, p.errorf("expected %s", name)
	}
	p.position++
	return token, nil
}
func (p *Parser) expectOperator(value string) error {
	if p.current().Kind != TokenOperator || p.current().Text != value {
		return p.errorf("expected %s", value)
	}
	p.position++
	return nil
}
func (p *Parser) errorf(format string, values ...any) error {
	return fmt.Errorf("SQL parse error at %d: %s", p.current().Position, fmt.Sprintf(format, values...))
}
func joinTokens(tokens []Token) string {
	var b strings.Builder
	for i, token := range tokens {
		if i > 0 && token.Kind != TokenRParen && token.Kind != TokenDot && tokens[i-1].Kind != TokenLParen && tokens[i-1].Kind != TokenDot {
			b.WriteByte(' ')
		}
		if token.Kind == TokenString {
			b.WriteString("'")
			b.WriteString(token.Text)
			b.WriteString("'")
		} else {
			b.WriteString(token.Text)
		}
	}
	return b.String()
}
