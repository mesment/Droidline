package dvrelay

import (
 "bytes"
 "log/slog"
 "io"
 "net/http"
 "net/http/httptest"
 "testing"
 "context"
 "strings"
 "time"

 "github.com/coder/websocket"
)

const devToken = "0123456789abcdef0123456789abcdef"

func TestFrameRoundTrip(t *testing.T) {
 b,err:=EncodeDataFrame("phone_1",[]byte{0,1,2,255})
 if err!=nil { t.Fatal(err) }
 id,data,err:=DecodeDataFrame(b)
 if err!=nil || id!="phone_1" || !bytes.Equal(data,[]byte{0,1,2,255}) { t.Fatalf("frame: %s %v %v",id,data,err) }
 for _,frame:=range [][]byte{{},{1},{1,1,0},{2,1,1,'x'},{1,2,1,'x'}} {
  if _,_,err:=DecodeDataFrame(frame);err==nil {t.Fatalf("accepted invalid frame %v",frame)}
 }
 if _,err:=EncodeDataFrame("bad!peer",nil);err==nil {t.Fatal("accepted invalid peer")}
 if _,err:=EncodeDataFrame("phone",make([]byte,MaxPayloadSize+1));err==nil {t.Fatal("accepted oversized payload")}
}

func TestDevelopmentRequiresExplicitPairs(t *testing.T) {
 _,err:=NewDevelopment(Config{DevToken:devToken},nil)
 if err==nil {t.Fatal("accepted empty pairs")}
 _,err=NewDevelopment(Config{DevToken:"short",Pairs:[]Pair{{"phone","mac"}}},nil)
 if err==nil {t.Fatal("accepted weak token")}
 _,err=NewDevelopment(Config{DevToken:devToken,Pairs:[]Pair{{"phone","phone"}}},nil)
 if err==nil {t.Fatal("accepted self-pair")}
}

func TestRelayBinaryRoutingAndAuthorization(t *testing.T) {
 relay,err:=NewDevelopment(Config{DevToken:devToken,Pairs:[]Pair{{"phone","mac1"},{"phone","mac2"}}},slog.New(slog.NewTextHandler(io.Discard,nil)))
 if err!=nil {t.Fatal(err)}
 server:=httptest.NewServer(relay.Handler());defer server.Close()
 url:="ws"+strings.TrimPrefix(server.URL,"http")+"/v1/connect?peer="
 connect:=func(id,token string)(*websocket.Conn,error){
   ctx,cancel:=context.WithTimeout(context.Background(),3*time.Second);defer cancel()
   c,_,err:=websocket.Dial(ctx,url+id,&websocket.DialOptions{HTTPHeader:http.Header{"Authorization":[]string{"Bearer "+token}}})
   return c,err
 }
 if c,err:=connect("unknown",devToken);err==nil { c.CloseNow();t.Fatal("unauthorized identity connected") }
 if c,err:=connect("phone","bad");err==nil { c.CloseNow();t.Fatal("bad token connected") }
 phone,err:=connect("phone",devToken);if err!=nil {t.Fatal(err)};defer phone.CloseNow()
 mac1,err:=connect("mac1",devToken);if err!=nil {t.Fatal(err)};defer mac1.CloseNow()
 mac2,err:=connect("mac2",devToken);if err!=nil {t.Fatal(err)};defer mac2.CloseNow()
 send:=func(from *websocket.Conn,to string,payload []byte) {
   t.Helper(); b,err:=EncodeDataFrame(to,payload);if err!=nil {t.Fatal(err)}
   ctx,cancel:=context.WithTimeout(context.Background(),2*time.Second);defer cancel()
   if err:=from.Write(ctx,websocket.MessageBinary,b);err!=nil {t.Fatal(err)}
 }
 receive:=func(c *websocket.Conn,from string,data []byte) {
   t.Helper();ctx,cancel:=context.WithTimeout(context.Background(),2*time.Second);defer cancel()
   typ,b,err:=c.Read(ctx);if err!=nil {t.Fatal(err)}
   id,payload,err:=DecodeDataFrame(b)
   if err!=nil||typ!=websocket.MessageBinary||id!=from||!bytes.Equal(payload,data){t.Fatalf("unexpected frame: %s %v %v",id,payload,err)}
 }
 send(mac1,"phone",[]byte{0,255,1});receive(phone,"mac1",[]byte{0,255,1})
 send(phone,"mac2",[]byte("ok"));receive(mac2,"phone",[]byte("ok"))
 // Unapproved Mac-to-Mac messages are intentionally discarded.
 send(mac1,"mac2",[]byte("should not arrive"))
 // A subsequent authorized message must still be routed correctly.
 send(phone,"mac2",[]byte("allowed"));receive(mac2,"phone",[]byte("allowed"))
}
