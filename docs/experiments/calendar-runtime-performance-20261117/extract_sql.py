from pathlib import Path
src=Path('internal/storage/event_pages.go').read_text()
start=src.index('\tplaceholders :=',src.index('func queryCachedPageEvents('))
end=src.index('\n\tstart := time.Now()',start)
body=src[start:end].replace('calendar.DateLayout','"2006-01-02"').replace('return nil, 0, 0, 0, 0, 0, errors.New','return built{}, errors.New')
pre='''package main
import("context";"database/sql";"database/sql/driver";"encoding/json";"errors";"fmt";"os";"sort";"strings";"time";sqlite "modernc.org/sqlite")
const cachedPageMaxTimeBytes=128
const cachedPageMaxIDBytes=1024
const cachedPageMaxPayloadBytes=32<<10
const DialectSQLite=0
const DialectPostgres=1
type Store struct{dialect int}
type CachedPageRequest struct{CalendarIDs []string;Start,End time.Time;PageSize int}
type CachedPagePosition struct{AllDay bool;StartAt,StartDate,EventID,CalendarID string}
type built struct{Query string;Args []any;Projection,Scalar,Master string}
func build(s *Store,req CachedPageRequest,after *CachedPagePosition)(built,error){
'''
post='''
return built{query,args,projection,scalarOK,master},nil
}
func variant(b built,name string) string {
 q:=b.Query
 if strings.Contains(name,"cursor-first") {clause:=" AND COALESCE("+b.Master+",'')<>'seriesMaster'"; q=strings.Replace(q,clause,"",1);q=strings.Replace(q," ORDER BY e.start_at IS NULL",clause+" ORDER BY e.start_at IS NULL",1)}
 if strings.Contains(name,"late-project") {
 old:="("+b.Scalar+") AS scalar_ok,"+b.Projection+" AS summary_json"
 if !strings.Contains(q,old){panic("missing projection")}
 q=strings.Replace(q,old,"e.payload_json AS payload_json",1)
 boundary:=") SELECT "
 i:=strings.Index(q,boundary);if i<0{panic("missing CTE boundary")}
 q=q[:i+1]+", projected AS MATERIALIZED (SELECT e.*, ("+b.Scalar+") AS scalar_ok,"+b.Projection+" AS summary_json FROM selected e)"+q[i+1:]
 q=strings.Replace(q,"FROM selected ORDER BY","FROM projected ORDER BY",1)
 }
 return q
}
type result struct{Name string;WalkMS []float64;MedianMS float64;Pages,Rows int;JSONMasterEvaluations,JSONProjectionEvaluations int64;Plan []string}
func main(){
 if len(os.Args)!=3||!strings.HasPrefix(os.Args[1],"/private/tmp/calendar-r6-"){panic("explicit synthetic fixture required")}
 counters:=map[string]int64{}
 sqlite.MustRegisterScalarFunction("diag_count",1,func(_ *sqlite.FunctionContext,args []driver.Value)(driver.Value,error){counters[args[0].(string)]++;return int64(1),nil})
 db,err:=sql.Open("sqlite","file:"+os.Args[1]+"?mode=ro");if err!=nil{panic(err)};defer db.Close();db.SetMaxOpenConns(1)
 var version string;db.QueryRow("select sqlite_version()").Scan(&version)
 var id string;db.QueryRow("SELECT id FROM calendars ORDER BY id LIMIT 1").Scan(&id)
 req:=CachedPageRequest{[]string{id},time.Date(2026,11,17,0,0,0,0,time.UTC),time.Date(2026,11,18,0,0,0,0,time.UTC),25}
 names:=[]string{"current","cursor-first","late-project","cursor-first+late-project"};out:=[]result{}
 for _,name:=range names{
  r:=result{Name:name};var lastSignature string
  for round:=0;round<4;round++{
   var after *CachedPagePosition; started:=time.Now();pages,total:=0,0;var sig strings.Builder
   for {
    b,err:=build(&Store{},req,after);if err!=nil{panic(err)};q:=variant(b,name)
    rows,err:=db.QueryContext(context.Background(),q,b.Args...);if err!=nil{panic(err)}
    n:=0;for rows.Next(){v:=make([]any,9);p:=make([]any,9);for i:=range v{p[i]=&v[i]};if err:=rows.Scan(p...);err!=nil{panic(err)}
     if n<25{n++;total++;fmt.Fprint(&sig,v[0],"|",v[7],";");after=&CachedPagePosition{StartAt:fmt.Sprint(v[2]),EventID:fmt.Sprint(v[0]),CalendarID:fmt.Sprint(v[1])}}
    };if err:=rows.Err();err!=nil{panic(err)};rows.Close();pages++;if n<25{break}
   }
   if lastSignature!=""&&sig.String()!=lastSignature{panic("variant repeat parity failed")};lastSignature=sig.String()
   if round>0{r.WalkMS=append(r.WalkMS,float64(time.Since(started).Nanoseconds())/1e6)};r.Pages=pages;r.Rows=total
  }
  // Diagnostic counts use separate SQL executions and are excluded from timing.
  b,_:=build(&Store{},req,nil);q:=variant(b,name)
  q=strings.ReplaceAll(q,b.Master,"CASE WHEN diag_count('master') THEN "+b.Master+" END")
  q=strings.ReplaceAll(q,b.Projection,"CASE WHEN diag_count('projection') THEN "+b.Projection+" END")
  counters["master"]=0;counters["projection"]=0
  rows,err:=db.Query(q,b.Args...);if err!=nil{panic(err)};for rows.Next(){};rows.Close()
  r.JSONMasterEvaluations=counters["master"];r.JSONProjectionEvaluations=counters["projection"]
  plan,err:=db.Query("EXPLAIN QUERY PLAN "+variant(b,name),b.Args...);if err!=nil{panic(err)};for plan.Next(){var a,c,d int;var detail string;plan.Scan(&a,&c,&d,&detail);r.Plan=append(r.Plan,detail)};plan.Close()
  sorted:=append([]float64{},r.WalkMS...);sort.Float64s(sorted);r.MedianMS=sorted[len(sorted)/2];out=append(out,r)
  os.WriteFile(os.Args[2]+"/"+name+".sql",[]byte(variant(b,name)),0600)
  if name=="current"{os.WriteFile(os.Args[2]+"/reference-signature.txt",[]byte(lastSignature),0600)}else{ref,_:=os.ReadFile(os.Args[2]+"/reference-signature.txt");if string(ref)!=lastSignature{panic("cross-variant parity failed")}}
 }
 json.NewEncoder(os.Stdout).Encode(map[string]any{"sqlite_version":version,"synthetic_only":true,"rounds":3,"warmups":1,"description_bytes":512,"limit":26,"results":out,"note":"SQL-only isolated experiment; projected/key parity; no runtime changes, no HTTP/authority/background; terminal empty SQL call included"})
}
'''
Path('/private/tmp/calendar-r7-sql-diagnosis/main.go').write_text(pre+body+post)
