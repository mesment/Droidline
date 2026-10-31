// Command droidvia-relay is an explicitly local-only, development relay.
// It is NOT ready for Internet deployment: R03 must add real identity
// verification, route grants and DBP end-to-end session authentication.
package main

import (
 "flag"
 "fmt"
 "log/slog"
 "net"
 "net/http"
 "os"
 "strings"
 "time"

 "github.com/mesment/Droidline/droidvia-relay/internal/relay"
)

func main() {
 listen:=flag.String("listen","127.0.0.1:8767","loopback listening address (development only)")
 pairs:=flag.String("pairs","","comma-separated peer pairs: phone:mac1,phone:mac2")
 ticketFor:=flag.String("ticket-for","","print a per-device /v1/transfer development ticket and exit")
 flag.Parse()
 host,_,err:=net.SplitHostPort(*listen)
 if err!=nil||!(host=="127.0.0.1"||host=="::1"||host=="localhost") {
  fmt.Fprintln(os.Stderr,"development relay may only bind loopback")
  os.Exit(2)
 }
 token:=os.Getenv("DROIDVIA_DEV_RELAY_TOKEN")
 if *ticketFor!="" {
  if len(token)<32 {fmt.Fprintln(os.Stderr,"DROIDVIA_DEV_RELAY_TOKEN must have at least 32 characters");os.Exit(2)}
  if strings.ContainsAny(*ticketFor," /?&=\\t\\n") {fmt.Fprintln(os.Stderr,"invalid peer ID");os.Exit(2)}
  fmt.Println(relay.TransferDeviceTicket(token,*ticketFor))
  return
 }
 var p []relay.Pair
 for _,entry:=range strings.Split(*pairs,","){
  if entry=="" {continue}
  sides:=strings.Split(entry,":")
  if len(sides)!=2 {fmt.Fprintln(os.Stderr,"invalid peer pair");os.Exit(2)}
  p=append(p,relay.Pair{A:sides[0],B:sides[1]})
 }
 r,err:=relay.NewDevelopment(relay.Config{DevToken:token,Pairs:p},slog.Default())
 if err!=nil {fmt.Fprintln(os.Stderr,err);os.Exit(2)}
 srv:=&http.Server{Addr:*listen,Handler:r.Handler(),ReadHeaderTimeout:5*time.Second}
 slog.Info("development relay listening; DO NOT expose on the internet","addr",*listen)
 if err:=srv.ListenAndServe();err!=nil&&err!=http.ErrServerClosed {fmt.Fprintln(os.Stderr,err);os.Exit(1)}
}
