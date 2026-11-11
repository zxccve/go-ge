"""Dump zone packet fields."""
import ctypes, ctypes.wintypes as wt, struct, subprocess
PQI,PVR=0x0400,0x0010
class MBI(ctypes.Structure):
    _fields_=[("BaseAddress",ctypes.c_void_p),("AllocationBase",ctypes.c_void_p),("AllocationProtect",wt.DWORD),("RegionSize",ctypes.c_size_t),("State",wt.DWORD),("Protect",wt.DWORD),("Type",wt.DWORD)]
k32=ctypes.WinDLL("kernel32",use_last_error=True)
k32.OpenProcess.restype=wt.HANDLE
k32.VirtualQueryEx.argtypes=[wt.HANDLE,ctypes.c_void_p,ctypes.POINTER(MBI),ctypes.c_size_t]
k32.ReadProcessMemory.argtypes=[wt.HANDLE,ctypes.c_void_p,ctypes.c_void_p,ctypes.c_size_t,ctypes.POINTER(ctypes.c_size_t)]
def pid_of(n="ge.exe"):
    out=subprocess.run(["tasklist","/FI","IMAGENAME eq "+n,"/FO","CSV","/NH"],capture_output=True,text=True).stdout
    for line in out.splitlines():
        p=[x.strip('"') for x in line.split('","')]
        if len(p)>=2 and p[1].isdigit():
            return int(p[1])
    return None
def rd(h,a,n):
    b=ctypes.create_string_buffer(n); got=ctypes.c_size_t(0)
    k32.ReadProcessMemory(h,ctypes.c_void_p(a),b,n,ctypes.byref(got))
    return b.raw[:got.value]
import sys
h=k32.OpenProcess(PQI|PVR,False,pid_of())
nb=0xb77e3c
sb=nb+3276748
lo=int(sys.argv[1]) if len(sys.argv)>1 else 3590
hi=int(sys.argv[2]) if len(sys.argv)>2 else 3605
for c in range(lo,hi+1):
    nm=rd(h,nb+c*50,50).split(b"\x00")[0].decode("latin-1","replace")
    sz=struct.unpack("<i",rd(h,sb+c*4,4))[0]
    print("%-5d %-34s %d%s"%(c,nm,sz,"  (var)" if sz==0 else ""))
