package structure

import (
	"slices"
	"strings"
	"testing"
)

func find(t *testing.T, fs FileStructure, name string) Symbol {
	t.Helper()
	for _, s := range fs.Symbols {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("symbol %q not found in %+v", name, fs.Symbols)
	return Symbol{}
}

func names(fs FileStructure) []string {
	var out []string
	for _, s := range fs.Symbols {
		out = append(out, s.Kind+":"+s.Name)
	}
	return out
}

const goSource = `// Package store persists users.
package store

import (
	"context"
	"database/sql"
	"net/http"
)

// MaxUsers limits the table.
const MaxUsers = 100

var ErrNotFound = errors.New("not found")

// User is an account.
type User struct {
	ID    int64  ` + "`json:\"id\" db:\"id\"`" + `
	Email string
	*Base
}

// Repo loads users.
type Repo interface {
	Get(ctx context.Context, id int64) (User, error)
	io.Closer
}

type ID = int64

// NewStore opens a store.
//
// More details here.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Get returns a user.
func (s *Store) Get(ctx context.Context, id int64) (User, error) { return User{}, nil }

func helper() {}

func routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /users/{id}", nil)
	mux.Handle("/healthz", nil)
	mux.Handle("not-a-path", nil)
}
`

func TestGoUsesTheAST(t *testing.T) {
	fs := Extract("internal/store/store.go", "Go", []byte(goSource))
	if fs.Parser != ParserGoAST || fs.Package != "store" || fs.ParseError != "" {
		t.Fatalf("parser/package/error = %s/%s/%s", fs.Parser, fs.Package, fs.ParseError)
	}
	if !slices.Equal(fs.Imports, []string{"context", "database/sql", "net/http"}) {
		t.Errorf("imports = %v", fs.Imports)
	}

	newStore := find(t, fs, "NewStore")
	if newStore.Kind != KindFunction || !newStore.Exported || newStore.Signature != "func NewStore(db *sql.DB) *Store" {
		t.Errorf("NewStore = %+v", newStore)
	}
	if newStore.Doc != "NewStore opens a store." {
		t.Errorf("doc = %q (only the first paragraph is kept)", newStore.Doc)
	}
	get := find(t, fs, "Get")
	if get.Kind != KindMethod || get.Receiver != "*Store" || get.Signature != "func (s *Store) Get(ctx context.Context, id int64) (User, error)" {
		t.Errorf("Get = %+v", get)
	}
	if h := find(t, fs, "helper"); h.Exported {
		t.Error("helper should not be exported")
	}

	user := find(t, fs, "User")
	if user.Kind != KindStruct || !slices.Equal(user.Members, []string{"ID int64 `json:\"id\" db:\"id\"`", "Email string", "*Base"}) {
		t.Errorf("User = %+v", user)
	}
	repo := find(t, fs, "Repo")
	if repo.Kind != KindInterface || !slices.Equal(repo.Members, []string{"Get(ctx context.Context, id int64) (User, error)", "io.Closer"}) {
		t.Errorf("Repo = %+v", repo)
	}
	if id := find(t, fs, "ID"); id.Kind != KindType || id.Signature != "type ID int64" {
		t.Errorf("ID = %+v", id)
	}
	if c := find(t, fs, "MaxUsers"); c.Kind != KindConst || c.Doc != "MaxUsers limits the table." {
		t.Errorf("MaxUsers = %+v", c)
	}
	if v := find(t, fs, "ErrNotFound"); v.Kind != KindVar || !v.Exported {
		t.Errorf("ErrNotFound = %+v", v)
	}

	if len(fs.Routes) != 2 || fs.Routes[0] != (Route{Method: "GET", Path: "/users/{id}", Line: 41}) || fs.Routes[1].Path != "/healthz" || fs.Routes[1].Method != "" {
		t.Errorf("routes = %+v", fs.Routes)
	}
	n := fs.Names()
	if !n["Get"] || !n["Store.Get"] || !n["NewStore"] || n["missing"] {
		t.Errorf("Names = %v", n)
	}
	if fs.Lines != strings.Count(goSource, "\n") {
		t.Errorf("lines = %d", fs.Lines)
	}
}

func TestGoParseErrorsKeepPartialResults(t *testing.T) {
	fs := Extract("bad.go", "Go", []byte("package bad\n\nfunc Good() {}\n\nfunc broken( {\n"))
	if fs.ParseError == "" || fs.Package != "bad" {
		t.Fatalf("fs = %+v", fs)
	}
	if !slices.Contains(names(fs), "function:Good") {
		t.Errorf("partial symbols = %v", names(fs))
	}
}

func TestPython(t *testing.T) {
	src := `import os
from flask import Flask, jsonify
from .models import User

app = Flask(__name__)

class UserService(BaseService):
    def __init__(self, db):
        self.db = db

    async def get_user(self, user_id: int) -> User:
        return None

def _private():
    pass

@app.route("/users/<int:id>", methods=["GET"])
def show_user(id):
    return jsonify({})

@router.post("/items")
async def create_item(item):
    pass
`
	fs := Extract("app/service.py", "Python", []byte(src))
	if fs.Parser != ParserHeuristic {
		t.Errorf("parser = %s", fs.Parser)
	}
	if !slices.Equal(fs.Imports, []string{"os", "flask", ".models"}) {
		t.Errorf("imports = %v", fs.Imports)
	}
	want := []string{"class:UserService", "method:__init__", "method:get_user", "function:_private", "function:show_user", "function:create_item"}
	if !slices.Equal(names(fs), want) {
		t.Errorf("symbols = %v", names(fs))
	}
	if s := find(t, fs, "get_user"); s.Signature != "get_user(self, user_id: int)" {
		t.Errorf("signature = %q", s.Signature)
	}
	if find(t, fs, "_private").Exported || !find(t, fs, "show_user").Exported {
		t.Error("exported flags wrong")
	}
	if len(fs.Routes) != 2 || fs.Routes[0].Path != "/users/<int:id>" || fs.Routes[1] != (Route{Method: "POST", Path: "/items", Line: 21}) {
		t.Errorf("routes = %+v", fs.Routes)
	}
}

func TestJavaScriptAndTypeScript(t *testing.T) {
	src := `import express from 'express';
import { db } from "./db.js";
const fs = require('fs');

export interface Options { port: number }
export type Handler = (req: Request) => void;

export class Server extends Base {
  constructor(options) {
  }
  async start(port: number): Promise<void> {
  }
}

export async function createApp(config) {
  const app = express();
  app.get('/api/users', listUsers);
  router.post("/api/users", createUser);
  if (x) {
  }
}

export const listUsers = async (req, res) => {};
const helper = x => x * 2;
`
	fs := Extract("src/server.ts", "TypeScript", []byte(src))
	if !slices.Equal(fs.Imports, []string{"express", "./db.js", "fs"}) {
		t.Errorf("imports = %v", fs.Imports)
	}
	want := []string{"interface:Options", "type:Handler", "class:Server", "method:constructor", "method:start", "function:createApp", "function:listUsers", "function:helper"}
	if !slices.Equal(names(fs), want) {
		t.Errorf("symbols = %v", names(fs))
	}
	if !find(t, fs, "createApp").Exported || find(t, fs, "helper").Exported {
		t.Error("exported flags wrong")
	}
	if len(fs.Routes) != 2 || fs.Routes[0] != (Route{Method: "GET", Path: "/api/users", Line: 17}) || fs.Routes[1].Method != "POST" {
		t.Errorf("routes = %+v", fs.Routes)
	}
}

func TestJavaAndCSharp(t *testing.T) {
	java := `package com.example.api;

import java.util.List;
import org.springframework.web.bind.annotation.*;

@RestController
public class UserController {
    @GetMapping("/users")
    public List<User> list() {
        return service.findAll();
    }

    private void audit(String event) throws IOException {
    }
}

public interface UserRepository extends JpaRepository<User, Long> {}
`
	fs := Extract("src/main/java/UserController.java", "Java", []byte(java))
	if fs.Package != "com.example.api" || !slices.Equal(fs.Imports, []string{"java.util.List", "org.springframework.web.bind.annotation.*"}) {
		t.Errorf("package/imports = %s %v", fs.Package, fs.Imports)
	}
	if want := []string{"class:UserController", "method:list", "method:audit", "interface:UserRepository"}; !slices.Equal(names(fs), want) {
		t.Errorf("symbols = %v", names(fs))
	}
	if !find(t, fs, "list").Exported || find(t, fs, "audit").Exported {
		t.Error("exported flags wrong")
	}
	if len(fs.Routes) != 1 || fs.Routes[0].Method != "GET" || fs.Routes[0].Path != "/users" {
		t.Errorf("routes = %+v", fs.Routes)
	}

	cs := `using System;
namespace Shop.Api
{
    public sealed class OrdersController : ControllerBase
    {
        [HttpPost("orders")]
        public async Task<IActionResult> Create(OrderDto dto)
        {
        }
    }
}
`
	fs = Extract("OrdersController.cs", "C#", []byte(cs))
	if fs.Package != "Shop.Api" || !slices.Equal(fs.Imports, []string{"System"}) {
		t.Errorf("namespace/imports = %s %v", fs.Package, fs.Imports)
	}
	if want := []string{"class:OrdersController", "method:Create"}; !slices.Equal(names(fs), want) {
		t.Errorf("symbols = %v", names(fs))
	}
	if len(fs.Routes) != 1 || fs.Routes[0].Path != "orders" || fs.Routes[0].Method != "POST" {
		t.Errorf("routes = %+v", fs.Routes)
	}
}

func TestRustPHPRubyAndC(t *testing.T) {
	rust := "use std::collections::HashMap;\npub struct Cache { map: HashMap<String, String> }\npub trait Store {}\nenum State { A }\nimpl Cache {\n    pub fn new() -> Self { todo!() }\n    fn evict(&mut self) {}\n}\npub async fn serve(addr: &str) -> Result<()> {}\nconst MAX_SIZE: usize = 10;\n"
	fs := Extract("src/cache.rs", "Rust", []byte(rust))
	if want := []string{"struct:Cache", "interface:Store", "type:State", "method:new", "method:evict", "function:serve", "const:MAX_SIZE"}; !slices.Equal(names(fs), want) {
		t.Errorf("rust symbols = %v", names(fs))
	}
	if !find(t, fs, "new").Exported || find(t, fs, "evict").Exported {
		t.Error("rust exported flags wrong")
	}

	php := "<?php\nnamespace App\\Http;\nuse Illuminate\\Support\\Facades\\Route;\nclass UserController extends Controller {\n    public function index(Request $r) {}\n    private function check() {}\n}\nfunction helper($x) {}\nRoute::get('/users', [UserController::class, 'index']);\n"
	fs = Extract("app/Http/UserController.php", "PHP", []byte(php))
	if want := []string{"class:UserController", "method:index", "method:check", "function:helper"}; !slices.Equal(names(fs), want) {
		t.Errorf("php symbols = %v", names(fs))
	}
	if fs.Package != `App\Http` || len(fs.Routes) != 1 || fs.Routes[0].Path != "/users" {
		t.Errorf("php namespace/routes = %s %+v", fs.Package, fs.Routes)
	}

	ruby := "require 'json'\nmodule Shop\n  class Order\n    def total(items)\n    end\n    def self.build; end\n    def valid?\n    end\n  end\nend\n"
	fs = Extract("lib/order.rb", "Ruby", []byte(ruby))
	if want := []string{"module:Shop", "class:Order", "method:total", "method:build", "method:valid?"}; !slices.Equal(names(fs), want) {
		t.Errorf("ruby symbols = %v", names(fs))
	}

	c := "#include <stdio.h>\n#include \"util.h\"\nstruct point {\n  int x;\n};\nstatic int add(int a, int b)\n{\n  if (a) {\n  }\n  return a + b;\n}\nint main(int argc, char **argv) {\n  return 0;\n}\n"
	fs = Extract("src/main.c", "C", []byte(c))
	if !slices.Equal(fs.Imports, []string{"stdio.h", "util.h"}) {
		t.Errorf("c imports = %v", fs.Imports)
	}
	if want := []string{"struct:point", "function:add", "function:main"}; !slices.Equal(names(fs), want) {
		t.Errorf("c symbols = %v", names(fs))
	}
	if find(t, fs, "add").Exported || !find(t, fs, "main").Exported {
		t.Error("c exported flags wrong")
	}
}

func TestSQLTables(t *testing.T) {
	sql := "CREATE TABLE users (id INTEGER);\ncreate table if not exists \"orders\" (id int);\nCREATE INDEX idx ON users(id);\n"
	fs := Extract("db/schema.sql", "SQL", []byte(sql))
	if want := []string{"table:users", "table:orders"}; !slices.Equal(names(fs), want) {
		t.Errorf("tables = %v", names(fs))
	}
}

func TestManifests(t *testing.T) {
	cases := []struct {
		path, content string
		want          []string
	}{
		{"go.mod", "module x\n\ngo 1.22\n\nrequire (\n\tgolang.org/x/crypto v0.20.0\n\tgithub.com/google/uuid v1.6.0 // indirect\n)\n\nrequire modernc.org/sqlite v1.30.0\n", []string{"golang.org/x/crypto", "github.com/google/uuid (indirect)", "modernc.org/sqlite"}},
		{"web/package.json", `{"dependencies":{"react":"^18","express":"4"},"devDependencies":{"jest":"29"}}`, []string{"express", "react", "jest (dev)"}},
		{"requirements.txt", "# web\nflask==3.0\nrequests>=2\n-r other.txt\nuvicorn[standard]\n", []string{"flask", "requests", "uvicorn"}},
		{"Cargo.toml", "[package]\nname = \"x\"\n\n[dependencies]\nserde = \"1\"\ntokio = { version = \"1\" }\n\n[dev-dependencies]\ntempfile = \"3\"\n", []string{"serde", "tokio", "tempfile"}},
		{"pyproject.toml", "[project]\nname='x'\n[tool.poetry.dependencies]\npython = \"^3.10\"\nfastapi = \"*\"\n", []string{"fastapi"}},
		{"composer.json", `{"require":{"php":">=8","laravel/framework":"^10","ext-json":"*"}}`, []string{"laravel/framework"}},
		{"Gemfile", "source 'https://rubygems.org'\ngem 'rails', '~> 7'\ngem \"pg\"\n", []string{"rails", "pg"}},
		{"pom.xml", "<project><artifactId>app</artifactId><dependencies><dependency><artifactId>spring-web</artifactId></dependency></dependencies></project>", []string{"app", "spring-web"}},
		{"App.csproj", `<ItemGroup><PackageReference Include="Serilog" Version="3" /></ItemGroup>`, []string{"Serilog"}},
	}
	for _, c := range cases {
		fs := Extract(c.path, "", []byte(c.content))
		if fs.Parser != ParserManifest || !slices.Equal(fs.Dependencies, c.want) {
			t.Errorf("%s: parser %s deps %v, want %v", c.path, fs.Parser, fs.Dependencies, c.want)
		}
	}
	if fs := Extract("package.json", "", []byte("{broken")); fs.Parser != ParserManifest || fs.Dependencies != nil {
		t.Errorf("broken package.json = %+v", fs)
	}
}

func TestUnknownLanguagesAndSummary(t *testing.T) {
	fs := Extract("notes.md", "Markdown", []byte("# Title\n\ntext\n"))
	if fs.Parser != ParserNone || len(fs.Symbols) != 0 || fs.Lines != 3 {
		t.Errorf("markdown = %+v", fs)
	}

	g := Extract("store.go", "Go", []byte(goSource))
	summary := g.Summary()
	for _, want := range []string{"parser: go/ast", "package: store", "imports: context, database/sql, net/http",
		"- method func (s *Store) Get(ctx context.Context, id int64) (User, error)", "- struct User {ID int64", "- route GET /users/{id}"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestSymbolLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("package big\n")
	for i := 0; i < MaxSymbols+50; i++ {
		b.WriteString("func F" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + "() {}\n")
	}
	if fs := Extract("big.go", "Go", []byte(b.String())); len(fs.Symbols) != MaxSymbols {
		t.Errorf("symbols = %d", len(fs.Symbols))
	}
}
