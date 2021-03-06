package main

import (
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"./sessions"
	"github.com/garyburd/redigo/redis"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/mux"
	"github.com/gorilla/securecookie"
	goCache "github.com/pmylund/go-cache"
	"github.com/russross/blackfriday"
)

const (
	maxConnectionCount = 256
	memosPerPage       = 100
	listenAddr         = ":5000"
	sessionName        = "isucon_session"
	tmpDir             = "/tmp/"
	dbConnPoolSize     = 10
	memcachedServer    = "localhost:11212"
	sessionSecret      = "kH<{11qpic*gf0e21YK7YtwyUvE9l<1r>yX8R-Op"
)

type Config struct {
	Database struct {
		Dbname   string `json:"dbname"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"database"`
}

type User struct {
	Id         int
	Username   string
	Password   string
	Salt       string
	LastAccess string
}

type Memo struct {
	Id        int
	User      int
	Content   string
	IsPrivate int
	CreatedAt string
	UpdatedAt string
	Username  string
}

type Memos []*Memo

type View struct {
	User      *User
	Memo      *Memo
	Memos     *Memos
	Page      int
	PageStart int
	PageEnd   int
	Total     int
	Older     *Memo
	Newer     *Memo
	Session   *sessions.Session
}

var (
	dbConnPool chan *sql.DB
	baseUrl    *url.URL
	fmap       = template.FuncMap{
		"url_for": func(path string) string {
			return baseUrl.String() + path
		},
		"first_line": func(s string) string {
			sl := strings.Split(s, "\n")
			return sl[0]
		},
		"get_token": func(session *sessions.Session) interface{} {
			return session.Values["token"]
		},
		"gen_markdown": func(s string) template.HTML {
			h, found := getHTML(s)
			if found {
				return h
			}
			out := blackfriday.MarkdownCommon([]byte(s))
			return template.HTML(out)
		},
	}
	tmpl = template.Must(template.New("tmpl").Funcs(fmap).ParseGlob("templates/*.html"))
)

var port = flag.Uint("port", 0, "port to listen")
var gocache = goCache.New(30*time.Second, 10*time.Second)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()

	env := os.Getenv("ISUCON_ENV")
	if env == "" {
		env = "local"
	}
	config := loadConfig("../config/" + env + ".json")
	db := config.Database
	connectionString := fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s?charset=utf8",
		db.Username, db.Password, db.Host, db.Port, db.Dbname,
	)
	log.Printf("db: %s", connectionString)

	dbConnPool = make(chan *sql.DB, dbConnPoolSize)
	for i := 0; i < dbConnPoolSize; i++ {
		conn, err := sql.Open("mysql", connectionString)
		if err != nil {
			log.Panicf("Error opening database: %v", err)
		}
		dbConnPool <- conn
		defer conn.Close()
	}

	r := mux.NewRouter()
	r.HandleFunc("/", topHandler)
	r.HandleFunc("/signin", signinHandler).Methods("GET", "HEAD")
	r.HandleFunc("/signin", signinPostHandler).Methods("POST")
	r.HandleFunc("/signout", signoutHandler)
	r.HandleFunc("/mypage", mypageHandler)
	r.HandleFunc("/memo/{memo_id}", memoHandler).Methods("GET", "HEAD")
	r.HandleFunc("/memo", memoPostHandler).Methods("POST")
	r.HandleFunc("/recent/{page:[0-9]+}", recentHandler)
	r.HandleFunc("/init", initHandler)
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./public/")))
	http.Handle("/", r)

	sigchan := make(chan os.Signal)
	signal.Notify(sigchan, os.Interrupt)
	signal.Notify(sigchan, syscall.SIGTERM)
	signal.Notify(sigchan, syscall.SIGINT)

	var l net.Listener
	var err error
	if *port == 0 {
		ferr := os.Remove("/tmp/server.sock")
		if ferr != nil {
			if !os.IsNotExist(ferr) {
				panic(ferr.Error())
			}
		}
		l, err = net.Listen("unix", "/tmp/server.sock")
		os.Chmod("/tmp/server.sock", 0777)
	} else {
		l, err = net.ListenTCP("tcp", &net.TCPAddr{Port: int(*port)})
	}
	if err != nil {
		panic(err.Error())
	}
	go func() {
		log.Println(http.Serve(l, nil))
	}()

	<-sigchan
}

func loadConfig(filename string) *Config {
	log.Printf("loading config file: %s", filename)
	f, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
	var config Config
	err = json.Unmarshal(f, &config)
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
	return &config
}

func prepareHandler(w http.ResponseWriter, r *http.Request) {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		baseUrl, _ = url.Parse("http://" + h)
	} else {
		baseUrl, _ = url.Parse("http://" + r.Host)
	}
}

func loadSession(w http.ResponseWriter, r *http.Request) (session *sessions.Session, err error) {
	store := sessions.NewMemcacheStore(memcachedServer, []byte(sessionSecret))
	return store.Get(r, sessionName)
}

func getUser(w http.ResponseWriter, r *http.Request, dbConn *sql.DB, session *sessions.Session) *User {
	userId := session.Values["user_id"]
	if userId == nil {
		return nil
	}
	user := &User{}
	rows, err := dbConn.Query("SELECT * FROM users WHERE id=?", userId)
	if err != nil {
		serverError(w, err)
		return nil
	}
	if rows.Next() {
		rows.Scan(&user.Id, &user.Username, &user.Password, &user.Salt, &user.LastAccess)
		rows.Close()
	}
	if user != nil {
		w.Header().Add("Cache-Control", "private")
	}
	return user
}

func antiCSRF(w http.ResponseWriter, r *http.Request, session *sessions.Session) bool {
	if r.FormValue("sid") != session.Values["token"] {
		code := http.StatusBadRequest
		http.Error(w, http.StatusText(code), code)
		return true
	}
	return false
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %s", err)
	code := http.StatusInternalServerError
	http.Error(w, http.StatusText(code), code)
}

func notFound(w http.ResponseWriter) {
	code := http.StatusNotFound
	http.Error(w, http.StatusText(code), code)
}

func topHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()
	user := getUser(w, r, dbConn, session)

	var totalCount int
	rdb, err := connectRedis()
	defer rdb.Close()
	if err != nil {
		serverError(w, err)
		return
	}

	memoIds, err := redis.Strings(rdb.Do("LRANGE", "public_memo_list", 0, memosPerPage-1))
	x, found := gocache.Get("public_memo_count")
	if found {
		// fmt.Println("HIT")
		totalCount = x.(int)
	} else {
		// fmt.Println("NO HIT")
		totalCount, err = redis.Int(rdb.Do("LLEN", "public_memo_list"))
		if err != nil {
			serverError(w, err)
			return
		}
		gocache.Set("public_memo_count", totalCount, 30*time.Second)
	}
	memos, err := lookupMemoMulti(dbConn, memoIds)
	if err != nil {
		serverError(w, err)
		return
	}

	v := &View{
		Total:     totalCount,
		Page:      0,
		PageStart: 1,
		PageEnd:   memosPerPage,
		Memos:     &memos,
		User:      user,
		Session:   session,
	}
	if err = tmpl.ExecuteTemplate(w, "index", v); err != nil {
		serverError(w, err)
	}
}

func recentHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()
	user := getUser(w, r, dbConn, session)
	vars := mux.Vars(r)
	page, _ := strconv.Atoi(vars["page"])

	rdb, err := connectRedis()
	if err != nil {
		serverError(w, err)
		return
	}
	defer rdb.Close()
	memoIds, err := redis.Strings(rdb.Do("LRANGE", "public_memo_list", memosPerPage*page, memosPerPage*(page+1)-1))
	if err != nil {
		serverError(w, err)
		return
	}
	memos, err := lookupMemoMulti(dbConn, memoIds)
	if err != nil {
		serverError(w, err)
		return
	}

	var totalCount int
	x, found := gocache.Get("public_memo_count")
	if found {
		// fmt.Println("HIT")
		totalCount = x.(int)
	} else {
		totalCount, err = redis.Int(rdb.Do("LLEN", "public_memo_list"))
		if err != nil {
			serverError(w, err)
		}
		gocache.Set("public_memo_count", totalCount, 30*time.Second)
	}

	if len(memos) == 0 {
		notFound(w)
		return
	}

	v := &View{
		Total:     totalCount,
		Page:      page,
		PageStart: memosPerPage*page + 1,
		PageEnd:   memosPerPage * (page + 1),
		Memos:     &memos,
		User:      user,
		Session:   session,
	}
	if err = tmpl.ExecuteTemplate(w, "index", v); err != nil {
		serverError(w, err)
	}
}

func initHandler(w http.ResponseWriter, r *http.Request) {
	gocache.Flush()

	err := initNames()
	if err != nil {
		serverError(w, err)
	}

	err = migrateToRedis()
	if err != nil {
		serverError(w, err)
	}

	w.Write([]byte("ok"))
}

var names [500]string

func initNames() error {
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()

	rows, err := dbConn.Query("SELECT id, username FROM users ORDER BY id ASC")
	if err != nil {
		return err
	}
	for rows.Next() {
		var Id int
		var Name string
		rows.Scan(&Id, &Name)
		log.Printf("%d\t%s", Id, Name)
		names[Id] = Name
	}
	rows.Close()
	return nil
}

func getUserName(id int) string {
	return names[id]
}

func signinHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()
	user := getUser(w, r, dbConn, session)

	v := &View{
		User:    user,
		Session: session,
	}
	if err := tmpl.ExecuteTemplate(w, "signin", v); err != nil {
		serverError(w, err)
		return
	}
}

func signinPostHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()

	username := r.FormValue("username")
	password := r.FormValue("password")
	user := &User{}
	rows, err := dbConn.Query("SELECT id, username, password, salt FROM users WHERE username=?", username)
	if err != nil {
		serverError(w, err)
		return
	}
	if rows.Next() {
		rows.Scan(&user.Id, &user.Username, &user.Password, &user.Salt)
	}
	rows.Close()
	if user.Id > 0 {
		h := sha256.New()
		h.Write([]byte(user.Salt + password))
		if user.Password == fmt.Sprintf("%x", h.Sum(nil)) {
			session.Values["user_id"] = user.Id
			session.Values["token"] = fmt.Sprintf("%x", securecookie.GenerateRandomKey(32))
			if err := session.Save(r, w); err != nil {
				serverError(w, err)
				return
			}
			if _, err := dbConn.Exec("UPDATE users SET last_access=now() WHERE id=?", user.Id); err != nil {
				serverError(w, err)
				return
			} else {
				http.Redirect(w, r, "/mypage", http.StatusFound)
			}
			return
		}
	}
	v := &View{
		Session: session,
	}
	if err := tmpl.ExecuteTemplate(w, "signin", v); err != nil {
		serverError(w, err)
		return
	}
}

func signoutHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	if antiCSRF(w, r, session) {
		return
	}

	http.SetCookie(w, sessions.NewCookie(sessionName, "", &sessions.Options{MaxAge: -1}))
	http.Redirect(w, r, "/", http.StatusFound)
}

func mypageHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()

	user := getUser(w, r, dbConn, session)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	rdb, err := connectRedis()
	if err != nil {
		serverError(w, err)
		return
	}
	defer rdb.Close()
	memoIds, err := redis.Strings(rdb.Do("LRANGE", fmt.Sprintf("user_memo_list:%d", user.Id), 0, -1))
	if err != nil {
		serverError(w, err)
		return
	}
	memos, err := lookupMemoMulti(dbConn, memoIds)
	if err != nil {
		serverError(w, err)
		return
	}
	v := &View{
		Memos:   &memos,
		User:    user,
		Session: session,
	}
	if err = tmpl.ExecuteTemplate(w, "mypage", v); err != nil {
		serverError(w, err)
	}
}

func memoHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	vars := mux.Vars(r)
	memoId := vars["memo_id"]
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()
	user := getUser(w, r, dbConn, session)

	rows, err := dbConn.Query("SELECT id, user, content, is_private, created_at, updated_at FROM memos WHERE id=?", memoId)
	if err != nil {
		serverError(w, err)
		return
	}
	memo := &Memo{}
	if rows.Next() {
		rows.Scan(&memo.Id, &memo.User, &memo.Content, &memo.IsPrivate, &memo.CreatedAt, &memo.UpdatedAt)
		rows.Close()
	} else {
		notFound(w)
		return
	}
	if memo.IsPrivate == 1 {
		if user == nil || user.Id != memo.User {
			notFound(w)
			return
		}
	}
	memo.Username = getUserName(memo.User)

	memos := make(Memos, 0)
	if user != nil && user.Id == memo.User {
		rdb, err := connectRedis()
		defer rdb.Close()
		if err != nil {
			serverError(w, err)
			return
		}
		memoIds, err := redis.Strings(rdb.Do("LRANGE", fmt.Sprintf("user_memo_list:%d", user.Id), 0, -1))
		if err != nil {
			serverError(w, err)
			return
		}
		memos, err = lookupMemoMulti(dbConn, memoIds)
		if err != nil {
			serverError(w, err)
			return
		}
	} else {
		cond := "AND is_private=0"
		rows, err = dbConn.Query("SELECT id, content, is_private, created_at, updated_at FROM memos WHERE user=? "+cond+" ORDER BY created_at", memo.User)
		if err != nil {
			serverError(w, err)
			return
		}
		for rows.Next() {
			m := Memo{}
			rows.Scan(&m.Id, &m.Content, &m.IsPrivate, &m.CreatedAt, &m.UpdatedAt)
			memos = append(memos, &m)
		}
		rows.Close()
	}
	var older *Memo
	var newer *Memo
	for i, m := range memos {
		if m.Id == memo.Id {
			if i > 0 {
				older = memos[i-1]
			}
			if i < len(memos)-1 {
				newer = memos[i+1]
			}
		}
	}

	v := &View{
		User:    user,
		Memo:    memo,
		Older:   older,
		Newer:   newer,
		Session: session,
	}
	if err = tmpl.ExecuteTemplate(w, "memo", v); err != nil {
		serverError(w, err)
	}
}

func memoPostHandler(w http.ResponseWriter, r *http.Request) {
	session, err := loadSession(w, r)
	if err != nil {
		serverError(w, err)
		return
	}
	prepareHandler(w, r)
	if antiCSRF(w, r, session) {
		return
	}
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()

	user := getUser(w, r, dbConn, session)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	var isPrivate int
	if r.FormValue("is_private") == "1" {
		isPrivate = 1
	} else {
		gocache.Increment("public_memo_count", 1)
		isPrivate = 0
	}
	result, err := dbConn.Exec(
		"INSERT INTO memos (user, content, is_private, created_at) VALUES (?, ?, ?, now())",
		user.Id, r.FormValue("content"), isPrivate,
	)
	if err != nil {
		serverError(w, err)
		return
	}
	newId, _ := result.LastInsertId()
	rdb, err := connectRedis()
	if err != nil {
		serverError(w, err)
		return
	}
	rdb.Send("MULTI")
	rdb.Send("RPUSH", fmt.Sprintf("user_memo_list:%d", user.Id), newId)
	if isPrivate == 0 {
		rdb.Send("LPUSH", "public_memo_list", newId)
		rdb.Send("LPUSH", fmt.Sprintf("user_public_memo_list:%d", user.Id), newId)
	}
	_, err = rdb.Do("EXEC")
	if err != nil {
		fmt.Errorf(err.Error())
	}
	cacheHTML(r.FormValue("content"))
	http.Redirect(w, r, fmt.Sprintf("/memo/%d", newId), http.StatusFound)
}

func connectRedis() (redis.Conn, error) {
	c, err := redis.Dial("tcp", ":6379")
	return c, err
}

func migrateToRedis() error {
	r, err := connectRedis()
	if err != nil {
		panic(err)
	}
	dbConn := <-dbConnPool
	defer func() {
		dbConnPool <- dbConn
	}()

	cursor := 0
	r.Do("FLUSHDB")
	for {
		rows, err := dbConn.Query("SELECT * FROM memos WHERE id > ? ORDER BY id ASC LIMIT 2000", cursor)
		if err != nil {
			return err
		}
		r.Send("MULTI")
		rowsCount := 0
		for rows.Next() {
			memo := Memo{}
			rows.Scan(&memo.Id, &memo.User, &memo.Content, &memo.IsPrivate, &memo.CreatedAt, &memo.UpdatedAt)
			if memo.IsPrivate == 0 {
				r.Send("LPUSH", "public_memo_list", memo.Id)
				r.Send("RPUSH", fmt.Sprintf("user_public_memo_list:%d", memo.User), memo.Id)
			}
			r.Send("RPUSH", fmt.Sprintf("user_memo_list:%d", memo.User), memo.Id)
			rowsCount++
			go cacheHTML(memo.Content)
		}
		_, err = r.Do("EXEC")
		if err != nil {
			fmt.Errorf(err.Error())
		}
		if rowsCount < 1000 {
			break
		}
		cursor += 1000
		rows.Close()
	}

	return nil
}

func mdCacheKye(md string) string {
	h := md5.New()
	io.WriteString(h, md)
	return string(h.Sum(nil))
}

func cacheHTML(md string) {
	out := blackfriday.MarkdownCommon([]byte(md))
	gocache.Set(mdCacheKye(md), template.HTML(out), 10000*time.Second)
}

func getHTML(md string) (template.HTML, bool) {
	cache, found := gocache.Get(mdCacheKye(md))
	if !found {
		log.Printf("HTML cache not found")
		return "", false
	}
	return cache.(template.HTML), true
}

func lookupMemoMulti(dbConn *sql.DB, memoIds []string) (Memos, error) {
	memos := make(Memos, 0)
	placeHolder := "0"
	args := []interface{}{}
	for _, id := range memoIds {
		placeHolder += "," + id
		args = append(args, id)
	}
	rows, err := dbConn.Query("SELECT * FROM memos WHERE id IN (" + placeHolder + ")")
	defer rows.Close()
	if err != nil {
		return memos, err
	}

	memberOf := make(map[string]Memo, 1000)
	for rows.Next() {
		memo := Memo{}
		rows.Scan(&memo.Id, &memo.User, &memo.Content, &memo.IsPrivate, &memo.CreatedAt, &memo.UpdatedAt)
		memo.Username = getUserName(memo.User)
		memos = append(memos, &memo)
		memberOf[fmt.Sprintf("%d", memo.Id)] = memo
	}

	results := make(Memos, 0)
	for _, id := range memoIds {
		if v, found := memberOf[id]; found {
			results = append(results, &v)
		}
	}
	return results, nil
}

func lookupUserNameMulti(dbConn *sql.DB, userIds []int) (map[int]string, error) {

	usernameOf := map[int]string{}
	placeHolder := "0"
	args := []interface{}{}
	for _, id := range userIds {
		placeHolder += fmt.Sprintf(",%d", id)
		args = append(args, id)
	}
	rows, err := dbConn.Query("SELECT id, username FROM users WHERE id IN (" + placeHolder + ")")
	if err != nil {
		return usernameOf, err
	}
	for rows.Next() {
		username := ""
		id := 0
		rows.Scan(&id, &username)
		usernameOf[id] = username
	}
	rows.Close()
	return usernameOf, err
}
