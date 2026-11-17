from pathlib import Path
s=Path('/private/tmp/calendar-r7-sql-diagnosis/main.go').read_text().split('func main(){')[0]
s=s.replace('"database/sql/driver";','').replace('"sort";','').replace('sqlite "modernc.org/sqlite"','_ "github.com/jackc/pgx/v5/stdlib"')
s+='''func pg(q string)string{n:=0;var b strings.Builder;for _,c:=range q{if c=='?'{n++;fmt.Fprintf(&b,"$%d",n)}else{b.WriteRune(c)}};return b.String()}
func main(){
 if len(os.Args)!=3||!strings.HasPrefix(os.Args[1],"postgres://calendar_r6@127.0.0.1:55467/calendar_r6_baseline?")||!strings.Contains(os.Args[1],"search_path=r7_diag_20261117"){panic("synthetic PG fixture only")}
 db,e:=sql.Open("pgx",os.Args[1]);if e!=nil{panic(e)};defer db.Close()
 rows,e:=db.Query("SELECT cal.id,c.id,c.authorization_revision,cal.detail_receipt FROM calendars cal JOIN connections c ON c.id=cal.connection_id ORDER BY cal.id");if e!=nil{panic(e)}
 ids:=[]string{};tickets:=[][]any{};for rows.Next(){var id,c,rev,receipt string;rows.Scan(&id,&c,&rev,&receipt);ids=append(ids,id);tickets=append(tickets,[]any{c,id,rev,receipt,true})};rows.Close()
 req:=CachedPageRequest{ids,time.Date(2026,11,17,0,0,0,0,time.UTC),time.Date(2026,11,18,0,0,0,0,time.UTC),25}
 out:=[]map[string]any{}
 var a time.Time;var eventID,calID string
 db.QueryRow("SELECT start_at,event_id,calendar_id FROM cached_events ORDER BY start_at,event_id,calendar_id OFFSET 2000 LIMIT 1").Scan(&a,&eventID,&calID)
 for _,position:=range []string{"first","middle"}{var after *CachedPagePosition;if position=="middle"{after=&CachedPagePosition{StartAt:a.Format(time.RFC3339Nano),EventID:eventID,CalendarID:calID}}
 b,e:=build(&Store{DialectPostgres},req,after);if e!=nil{panic(e)}
 for _,name:=range []string{"current","late-project"}{q:=pg(variant(b,name));times:=[]float64{};for i:=0;i<11;i++{start:=time.Now();rs,e:=db.Query(q,b.Args...);if e!=nil{panic(e)};for rs.Next(){v:=make([]any,9);p:=make([]any,9);for j:=range v{p[j]=&v[j]};if e:=rs.Scan(p...);e!=nil{panic(e)}};if e:=rs.Err();e!=nil{panic(e)};rs.Close();if i>0{times=append(times,float64(time.Since(start).Nanoseconds())/1e6)}}
 var plan []byte;e=db.QueryRow("EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+q,b.Args...).Scan(&plan);if e!=nil{panic(e)};os.WriteFile(os.Args[2]+"/pg-"+position+"-"+name+"-explain.json",plan,0600)
 out=append(out,map[string]any{"position":position,"variant":name,"query_ms":times})
 }}
 clause:=`SELECT COUNT(*) FROM connections c JOIN calendars cal ON cal.connection_id=c.id JOIN calendar_sync_state st ON st.calendar_id=cal.id WHERE c.id=? AND cal.id=? AND c.status='connected' AND c.authorization_revision=? AND cal.detail_receipt=? AND cal.can_read=? AND (st.last_error_code IS NULL OR st.last_error_code NOT IN ('auth','permission'))`
 vals:=[]string{};args:=[]any{};for _,t:=range tickets{vals=append(vals,"(?,?,?,?)");args=append(args,t[:4]...)}
 batched:=`WITH expected(connection_id,calendar_id,rev,receipt) AS (VALUES `+strings.Join(vals,",")+`) SELECT COUNT(*) FROM expected x JOIN connections c ON c.id=x.connection_id JOIN calendars cal ON cal.id=x.calendar_id AND cal.connection_id=c.id JOIN calendar_sync_state st ON st.calendar_id=cal.id WHERE c.status='connected' AND c.authorization_revision=x.rev AND cal.detail_receipt=x.receipt AND cal.can_read=true AND (st.last_error_code IS NULL OR st.last_error_code NOT IN ('auth','permission'))`
 auth:=map[string][]float64{}
 for round:=0;round<21;round++{for _,kind:=range []string{"sixteen_queries","single_query"}{start:=time.Now();tx,e:=db.BeginTx(context.Background(),&sql.TxOptions{ReadOnly:true,Isolation:sql.LevelRepeatableRead});if e!=nil{panic(e)};if kind=="sixteen_queries"{for _,t:=range tickets{var n int;if e:=tx.QueryRow(pg(clause),t...).Scan(&n);e!=nil||n!=1{panic(fmt.Sprint(e,n))}}}else{var n int;if e:=tx.QueryRow(pg(batched),args...).Scan(&n);e!=nil||n!=16{panic(fmt.Sprint(e,n))}};if e:=tx.Commit();e!=nil{panic(e)};if round>0{auth[kind]=append(auth[kind],float64(time.Since(start).Nanoseconds())/1e6)}}}
 json.NewEncoder(os.Stdout).Encode(map[string]any{"synthetic_only":true,"sources":16,"events":4000,"query_experiment":out,"authority_experiment_ms":auth,"note":"SQL microbenchmarks, no runtime implementation; warmups excluded; authority includes begin/commit; valid grants only, no negative contract validation"})
}
'''
Path('/private/tmp/calendar-r7-sql-diagnosis/pg.go').write_text(s)
