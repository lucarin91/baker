package filter

import (
	"fmt"
	"runtime"

	"github.com/AdRoll/baker"
	lua "github.com/yuin/gopher-lua"
)

// LUADesc describes the LUA filter
var LUADesc = baker.FilterDesc{
	Name:   "LUA",
	New:    NewLUA,
	Config: &LUAConfig{},
	Help:   `Run a baker filter defined in a lua script`,
}

type LUAConfig struct {
	Script     string `help:"Path to the lua script where the baker filter is defined" required:"true"`
	FilterName string `help:"Name of the lua function to run as baker filter" required:"true"`
}

type LUA struct {
	l       *lua.LState    // lua state used during all the baker filter lifetime
	ud      *lua.LUserData // pre-allocated (reused) userdata for the processed record
	luaFunc lua.LValue     // lua filter function
	luaNext *lua.LFunction // lua next function (reused)
	next    func(baker.Record)
}

func NewLUA(cfg baker.FilterParams) (baker.Filter, error) {
	dcfg := cfg.DecodedConfig.(*LUAConfig)

	l := lua.NewState()
	if err := l.DoFile(dcfg.Script); err != nil {
		return nil, fmt.Errorf("can't compile lua script %q: %v", dcfg.Script, err)
	}

	registerLUATypes(l, cfg.ComponentParams)

	luaFunc := l.GetGlobal(dcfg.FilterName)
	if luaFunc.Type() == lua.LTNil {
		return nil, fmt.Errorf("can't find lua filter %q in script %q", dcfg.FilterName, dcfg.Script)
	}

	// Preallocate the userdata we use to wrap the record passed to the filter.
	// We can do this since a single instance of a baker filter is only ever
	// processing a single record at a time, so we can reuse the lua userdata
	// structure for it. This reduces allocations.
	ud := l.NewUserData()
	l.SetMetatable(ud, l.GetTypeMetatable(luaRecordTypeName))

	f := &LUA{}

	// Preallocate the lua next function passed to the filter.
	luaNext := l.NewFunction(func(L *lua.LState) int {
		f.next(fastcheckLuaRecord(L, 1).r)
		return 0
	})

	f.l = l
	f.ud = ud
	f.luaNext = luaNext
	f.luaFunc = luaFunc

	runtime.SetFinalizer(f, func(f *LUA) { f.l.Close() })

	return f, nil
}

func registerLUATypes(l *lua.LState, comp baker.ComponentParams) {
	registerLUARecordType(l)

	l.SetGlobal("createRecord", l.NewFunction(func(L *lua.LState) int {
		rec := comp.CreateRecord()
		ud := recordToLua(l, rec)
		L.Push(ud)
		return 1
	}))

	l.SetGlobal("validateRecord", l.NewFunction(func(L *lua.LState) int {
		luar := fastcheckLuaRecord(l, 1)
		ok, fidx := comp.ValidateRecord(luar.r)
		l.Push(lua.LBool(ok))
		l.Push(lua.LNumber(fidx))
		return 2
	}))

	// Create the fields table.
	fields := l.NewTable()
	for i, n := range comp.FieldNames {
		fields.RawSetString(n, lua.LNumber(i))
	}
	l.SetGlobal("fieldNames", fields)
}

func (t *LUA) Stats() baker.FilterStats { return baker.FilterStats{} }

func (t *LUA) Process(rec baker.Record, next func(baker.Record)) {
	// Modify the record inside the pre-allocated user value
	t.ud.Value = &luaRecord{r: rec}

	// Set the next function which is called by the lua filter to the one
	// we just received.
	t.next = next

	err := t.l.CallByParam(lua.P{
		Fn:      t.luaFunc,
		NRet:    0,
		Protect: true,
	}, t.ud, t.luaNext)

	if err != nil {
		panic(err)
	}
}

// lua record methods

const luaRecordTypeName = "record"

func registerLUARecordType(L *lua.LState) {
	mt := L.NewTypeMetatable(luaRecordTypeName)
	L.SetGlobal(luaRecordTypeName, mt)
	L.SetField(mt, "__index", L.SetFuncs(L.NewTable(), luaRecordMethods))
}

func recordToLua(L *lua.LState, r baker.Record) *lua.LUserData {
	ud := L.NewUserData()
	ud.Value = &luaRecord{r: r}
	L.SetMetatable(ud, L.GetTypeMetatable(luaRecordTypeName))
	return ud
}

func checkLuaRecord(L *lua.LState, n int) *luaRecord {
	ud := L.CheckUserData(n)
	if v, ok := ud.Value.(*luaRecord); ok {
		return v
	}
	L.ArgError(n, fmt.Sprintf("record expected, got %#v", ud.Value))
	return nil
}

func fastcheckLuaRecord(L *lua.LState, n int) *luaRecord {
	return L.Get(n).(*lua.LUserData).Value.(*luaRecord)
}

var luaRecordMethods = map[string]lua.LGFunction{
	"get":   luaRecordGet,
	"set":   luaRecordSet,
	"copy":  luaRecordCopy,
	"clear": luaRecordClear,
}

type luaRecord struct {
	r baker.Record
}

// record:get(int) returns string
func luaRecordGet(L *lua.LState) int {
	luar := fastcheckLuaRecord(L, 1)
	fidx := L.CheckInt(2)

	buf := luar.r.Get(baker.FieldIndex(fidx))

	L.Push(lua.LString(string(buf)))
	return 1
}

// record:set(int, string)
func luaRecordSet(L *lua.LState) int {
	luar := fastcheckLuaRecord(L, 1)
	fidx := L.CheckInt(2)
	val := L.CheckString(3)

	luar.r.Set(baker.FieldIndex(fidx), []byte(val))

	return 0
}

// record:copy() record
func luaRecordCopy(L *lua.LState) int {
	luar := fastcheckLuaRecord(L, 1)

	cpy := luar.r.Copy()
	ud := recordToLua(L, cpy)
	L.Push(ud)

	return 1
}

// record:clear()
func luaRecordClear(L *lua.LState) int {
	luar := fastcheckLuaRecord(L, 1)
	luar.r.Clear()

	return 0
}