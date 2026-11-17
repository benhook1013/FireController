package main
import("flag";"os";"syscall";"time")
func main(){
 var lease string;var dev,ino uint64
 flag.String("data-dir","","");flag.String("report","","")
 flag.StringVar(&lease,"lease-file","","");flag.Uint64Var(&dev,"lease-device",0,"");flag.Uint64Var(&ino,"lease-inode",0,"");flag.Parse()
 f,e:=os.OpenFile(lease,os.O_RDWR,0);if e!=nil{os.Exit(21)};defer f.Close()
 var s syscall.Stat_t;if syscall.Fstat(int(f.Fd()),&s)!=nil||s.Dev!=dev||s.Ino!=ino{os.Exit(22)}
 if syscall.Flock(int(f.Fd()),syscall.LOCK_EX|syscall.LOCK_NB)!=nil{os.Exit(23)}
 time.Sleep(600*time.Millisecond);os.Exit(7)
}
