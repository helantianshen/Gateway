from pathlib import Path
import subprocess, os, json
root=Path(__file__).parent
bench='''package router
import("fmt";"testing")
func BenchmarkUniformHost(b *testing.B) {
 for _,n:=range []int{10,1000,10000} { for _,kind:=range []string{"exact","wildcard"} {
 b.Run(fmt.Sprintf("hosts_%d/%s",n,kind),func(b *testing.B){
 routes:=make([]CompileInput,n); hosts:=make([]string,n)
 for i:=range n { h:=fmt.Sprintf("h%05d.example.com",i); hosts[i]=h; pattern:=h; if kind=="wildcard" {pattern="*."+h;hosts[i]="api."+h}; routes[i]=CompileInput{RouteID:fmt.Sprint(i),Host:pattern,Method:"GET",Path:"/users/:id",Upstream:"mock"} }
 r,e:=Compile(routes);if e!=nil {b.Fatal(e)}
 for i,h:=range hosts {m,e:=r.Match(h,"GET",[]string{"users","42"});if e!=nil||m==nil||m.RouteID!=fmt.Sprint(i)||m.Params["id"]!="42" {b.Fatal("匹配结果错误",i)}}
 segs:=[]string{"users","42"};b.ReportAllocs();b.ResetTimer()
 for i:=0;i<b.N;i++ {r.Match(hosts[i%n],"GET",segs)}
 })
 }}
}
'''
for v in ['before','after']:
 (root/v/'internal/router/uniform_bench_test.go').write_text(bench)
 with (root/'results'/f'{v}-test.txt').open('w') as f:
  subprocess.run(['go','test','./...'],cwd=root/v,stdout=f,stderr=subprocess.STDOUT,check=True)
 with (root/'results'/f'{v}-race.txt').open('w') as f:
  subprocess.run(['go','test','-race','./internal/router','./internal/dataplane/gateway'],cwd=root/v,stdout=f,stderr=subprocess.STDOUT,check=True)
 subprocess.run(['go','test','-c','-o',str(root/f'{v}.test'),'./internal/router'],cwd=root/v,check=True)
 subprocess.run(['go','build','-o',str(root/f'{v}.gateway'),'./cmd/gateway'],cwd=root/v,check=True)
 print(v,'built and tested',flush=True)
commands=['uname -a','go version','go env GOOS GOARCH GOAMD64 GOEXPERIMENT CGO_ENABLED','lscpu','cat /proc/self/cgroup','cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor','ps -eo pid,comm,pcpu --sort=-pcpu | head -15']
with (root/'results/environment.txt').open('w') as f:
 for c in commands:
  f.write(c+'\n');f.flush();subprocess.run(c,shell=True,stdout=f,stderr=f)
