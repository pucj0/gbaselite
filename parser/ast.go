package parser

type Statement interface{ statement() }
type Query interface {
	Statement
	query()
}

type Empty struct{}

type ColumnDef struct {
	Name, Type           string
	SQLType              string
	Length               int
	Nullable             bool
	NullabilitySpecified bool
	HasDefault           bool
	Default              Literal
	DefaultExpression    string
	AutoIncrement        bool
	Comment              string
	OnUpdate             string
	PrimaryKey           bool
	Unique               bool
	Check                string
}
type CreateDatabase struct {
	Name        string
	IfNotExists bool
}
type DropDatabase struct {
	Name     string
	IfExists bool
}
type CreateTable struct {
	Name        string
	Columns     []ColumnDef
	PrimaryKey  []string
	Indexes     []IndexDef
	IfNotExists bool
	Comment     string
	ForeignKeys []ForeignKeyDef
	Checks      []CheckDef
}
type CreateTableLike struct {
	Name, Source string
	IfNotExists  bool
}
type CreateTableAs struct {
	Name        string
	IfNotExists bool
	Query       Query
}
type ForeignKeyDef struct {
	Name       string
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string
	OnUpdate   string
}
type CheckDef struct{ Name, Expression string }
type IndexDef struct {
	Name    string
	Columns []string
	Unique  bool
}
type DropTable struct {
	Names    []string
	IfExists bool
}
type CreateIndex struct {
	Name, Table string
	Columns     []string
	Unique      bool
	Primary     bool
}
type DropIndex struct{ Name, Table string }
type RenameIndex struct{ Table, OldName, NewName string }
type AlterColumn struct {
	Table   string
	OldName string
	Column  ColumnDef
}
type AlterColumnDefault struct {
	Table, Name       string
	Drop              bool
	Default           Literal
	DefaultExpression string
}
type AddColumn struct {
	Table       string
	Column      ColumnDef
	First       bool
	After       string
	IfNotExists bool
}
type DropColumn struct {
	Table    string
	Name     string
	IfExists bool
}
type RenameColumn struct{ Table, OldName, NewName string }
type AlterForeignKey struct {
	Table      string
	ForeignKey ForeignKeyDef
	Drop       bool
	Name       string
}
type AlterCheck struct {
	Table string
	Check CheckDef
	Drop  bool
	Name  string
}
type AlterTableBatch struct {
	Table   string
	Actions []Statement
}
type RenameTablePair struct{ From, To string }
type RenameTable struct{ Pairs []RenameTablePair }
type Explain struct{ Query Query }
type AlterTableComment struct {
	Table   string
	Comment string
}
type CreateView struct {
	Name             string
	Definition       string
	Columns          []string
	OrReplace        bool
	AlterOnly        bool
	HasCreateOptions bool
}
type DropView struct {
	Names    []string
	IfExists bool
}
type Insert struct {
	Table       string
	Columns     []string
	Values      [][]Literal
	SetValues   []Expr
	Select      Query
	Ignore      bool
	Replace     bool
	OnDuplicate []InsertAssignment
}
type InsertAssignment struct {
	Column string
	Value  Expr
}
type SelectItem struct{ Expression, Alias string }
type Order struct {
	Column string
	Desc   bool
}
type Join struct {
	Type       string
	Table      string
	TableAlias string
	Subquery   Query
	On         Expr
}
type Select struct {
	Items         []SelectItem
	Distinct      bool
	Table         string
	TableAlias    string
	Subquery      Query
	Joins         []Join
	Where         Expr
	GroupBy       []string
	Having        Expr
	OrderBy       []Order
	Limit, Offset int
	HasLimit      bool
	Locking       bool
}
type Union struct {
	Queries       []Select
	All           []bool
	OrderBy       []Order
	Limit, Offset int
	HasLimit      bool
}
type Update struct {
	Table       string
	TableAlias  string
	Joins       []Join
	Assignments []UpdateAssignment
	Where       Expr
	Limit       int
	HasLimit    bool
}
type UpdateAssignment struct {
	Column string
	Value  Expr
}
type Delete struct {
	Table      string
	TableAlias string
	Targets    []string
	Joins      []Join
	Where      Expr
	Limit      int
	HasLimit   bool
}
type Truncate struct{ Table string }
type Show struct {
	What, Name string
	Full       bool
	Pattern    string
	Where      Expr
}
type Use struct{ Database string }
type Begin struct{}
type Commit struct{}
type Rollback struct{}
type Account struct{ Username, Host string }
type UserSpec struct {
	Account  Account
	Password string
}
type CreateUser struct {
	Users       []UserSpec
	IfNotExists bool
}
type AlterUser struct {
	Users    []UserSpec
	IfExists bool
}
type DropUser struct {
	Accounts []Account
	IfExists bool
}
type RenameUserPair struct{ From, To Account }
type RenameUser struct{ Pairs []RenameUserPair }
type SetPassword struct {
	Account  Account
	Password string
}
type Grant struct {
	Privileges  []string
	Database    string
	Table       string
	Accounts    []Account
	GrantOption bool
}
type Revoke struct {
	Privileges      []string
	Database        string
	Table           string
	Accounts        []Account
	GrantOptionOnly bool
}
type ShowGrants struct {
	Account    Account
	ForAccount bool
}
type ShowCreateUser struct{ Account Account }
type ExportDatabase struct{ Name, Path string }
type WithRecursive struct {
	Name      string
	Seed      Select
	Recursive Select
	Query     Select
}
type CommonTableExpression struct {
	Name    string
	Columns []string
	Query   Query
}
type With struct {
	Expressions []CommonTableExpression
	Query       Query
}

func (CreateDatabase) statement()     {}
func (Empty) statement()              {}
func (DropDatabase) statement()       {}
func (CreateTable) statement()        {}
func (CreateTableLike) statement()    {}
func (CreateTableAs) statement()      {}
func (DropTable) statement()          {}
func (CreateIndex) statement()        {}
func (DropIndex) statement()          {}
func (RenameIndex) statement()        {}
func (AlterColumn) statement()        {}
func (AlterColumnDefault) statement() {}
func (AddColumn) statement()          {}
func (DropColumn) statement()         {}
func (RenameColumn) statement()       {}
func (AlterForeignKey) statement()    {}
func (AlterCheck) statement()         {}
func (AlterTableBatch) statement()    {}
func (RenameTable) statement()        {}
func (Explain) statement()            {}
func (AlterTableComment) statement()  {}
func (CreateView) statement()         {}
func (DropView) statement()           {}
func (Insert) statement()             {}
func (Select) statement()             {}
func (Union) statement()              {}
func (Update) statement()             {}
func (Delete) statement()             {}
func (Truncate) statement()           {}
func (Show) statement()               {}
func (Use) statement()                {}
func (Begin) statement()              {}
func (Commit) statement()             {}
func (Rollback) statement()           {}
func (CreateUser) statement()         {}
func (AlterUser) statement()          {}
func (DropUser) statement()           {}
func (RenameUser) statement()         {}
func (SetPassword) statement()        {}
func (Grant) statement()              {}
func (Revoke) statement()             {}
func (ShowGrants) statement()         {}
func (ShowCreateUser) statement()     {}
func (ExportDatabase) statement()     {}
func (WithRecursive) statement()      {}
func (With) statement()               {}

func (Select) query() {}
func (Union) query()  {}

type Expr interface{ expression() }
type BinaryExpr struct {
	Left     Expr
	Operator string
	Right    Expr
}
type Identifier struct{ Name string }
type LiteralExpr struct{ Value Literal }
type RowExpr struct{ Values []Expr }
type UnaryExpr struct {
	Operator string
	Value    Expr
}
type InExpr struct {
	Value    Expr
	Values   []Expr
	Subquery Query
	Not      bool
}
type BetweenExpr struct {
	Value, Lower, Upper Expr
	Not                 bool
}
type IsExpr struct {
	Value, Target Expr
	Not           bool
}
type FunctionExpr struct {
	Name string
	Args []Expr
	Star bool
}
type IntervalExpr struct {
	Value Expr
	Unit  string
}
type WindowOrder struct {
	Expression Expr
	Desc       bool
}
type WindowExpr struct {
	Function    FunctionExpr
	PartitionBy []Expr
	OrderBy     []WindowOrder
}
type ScalarSubquery struct{ Query Query }
type ExistsExpr struct{ Query Query }
type CaseExpr struct {
	Operand Expr
	Whens   []CaseWhen
	Else    Expr
}
type CaseWhen struct{ When, Then Expr }

func (BinaryExpr) expression()     {}
func (Identifier) expression()     {}
func (LiteralExpr) expression()    {}
func (RowExpr) expression()        {}
func (UnaryExpr) expression()      {}
func (InExpr) expression()         {}
func (BetweenExpr) expression()    {}
func (IsExpr) expression()         {}
func (FunctionExpr) expression()   {}
func (IntervalExpr) expression()   {}
func (WindowExpr) expression()     {}
func (ScalarSubquery) expression() {}
func (ExistsExpr) expression()     {}
func (CaseExpr) expression()       {}

type LiteralKind uint8

const (
	LiteralNull LiteralKind = iota
	LiteralString
	LiteralNumber
	LiteralBoolean
)

type Literal struct {
	Kind LiteralKind
	Text string
}
