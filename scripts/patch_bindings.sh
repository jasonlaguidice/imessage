#!/bin/bash
# Patch uniffi-generated Go bindings for Go 1.24+ compatibility.
#
# Go 1.24+ disallows methods on type aliases of cgo types.
# This script converts `type RustBuffer = C.RustBuffer` to a named struct
# with unsafe.Pointer-based zero-copy conversion functions.
set -e

FILE="$1"
if [ -z "$FILE" ]; then
    FILE="pkg/rustpushgo/rustpushgo.go"
fi

echo "Patching $FILE for Go 1.24+ compatibility..."

# 1. Add CGO LDFLAGS
sed -i '' 's|// #include <rustpushgo.h>|// #include <rustpushgo.h>\n// #cgo LDFLAGS: -L${SRCDIR}/../../ -lrustpushgo -ldl -lm -framework Security -framework SystemConfiguration -framework CoreFoundation -framework Foundation -lz -lresolv|' "$FILE"

# 2. Replace type alias with a compatible named struct + conversion functions
python3 << 'PYEOF'
import sys, re

FILE = sys.argv[1]
with open(FILE) as f:
    content = f.read()

# Replace the type alias with a named struct
old = 'type RustBuffer = C.RustBuffer'
new = '''type RustBuffer struct {
	capacity C.int32_t
	len      C.int32_t
	data     *C.uint8_t
}

// Zero-copy conversion between RustBuffer and C.RustBuffer.
// These structs have identical memory layout.
func rustBufferToC(rb RustBuffer) C.RustBuffer {
	return *(*C.RustBuffer)(unsafe.Pointer(&rb))
}

func rustBufferFromC(crb C.RustBuffer) RustBuffer {
	return *(*RustBuffer)(unsafe.Pointer(&crb))
}'''
content = content.replace(old, new, 1)

# Fix RustBuffer.Free() - cb passed to C function
content = content.replace(
    'C.ffi_rustpushgo_rustbuffer_free(cb, status)',
    'C.ffi_rustpushgo_rustbuffer_free(rustBufferToC(cb), status)'
)

# Fix bytesToRustBuffer/stringToRustBuffer - C alloc returns C.RustBuffer
content = content.replace(
    'return C.ffi_rustpushgo_rustbuffer_from_bytes(',
    'return rustBufferFromC(C.ffi_rustpushgo_rustbuffer_from_bytes('
)
# Close the extra paren - find the full statement
content = re.sub(
    r'return rustBufferFromC\(C\.ffi_rustpushgo_rustbuffer_from_bytes\(foreign, status\)\)',
    'return rustBufferFromC(C.ffi_rustpushgo_rustbuffer_from_bytes(foreign, status))',
    content
)

# Fix status.errorBuf - it's C.RustBuffer, needs to be RustBufferI
content = content.replace(
    'converter.Lift(status.errorBuf)',
    'converter.Lift(rustBufferFromC(status.errorBuf))'
)
content = content.replace(
    'FfiConverterStringINSTANCE.Lift(status.errorBuf)',
    'FfiConverterStringINSTANCE.Lift(rustBufferFromC(status.errorBuf))'
)

# Fix all C.uniffi_rustpushgo_ function calls
# Pattern: These C functions may take C.RustBuffer args (from .Lower() which returns RustBuffer)
# and return C.RustBuffer (which needs to become RustBuffer)

# Strategy: 
# 1. All .Lower() calls that are passed as arguments to C functions need rustBufferToC()
# 2. All C function calls that are inside rustCall[RustBuffer] or rustCallWithError[RustBuffer] 
#    lambdas need their return value wrapped in rustBufferFromC()

# For (1): Find C function calls and wrap RustBuffer arguments
# The generated patterns look like:
#   C.uniffi_rustpushgo_fn_xxx(arg1, FfiConverterXxx.Lower(val), arg2, ...)
# where .Lower() returns RustBuffer but C function wants C.RustBuffer

# For (2): Functions called in `rustCall[RustBuffer]` or `rustCallWithError[RustBuffer]`
# lambdas that return the result of a C function call

# Let's do this line-by-line
lines = content.split('\n')
result = []
in_rustbuffer_lambda = False

for i, line in enumerate(lines):
    stripped = line.strip()
    
    # Track if we're inside a lambda that returns RustBuffer
    if 'func(status *C.RustCallStatus) RustBuffer {' in stripped:
        in_rustbuffer_lambda = True
    
    # Fix: C function calls inside RustBuffer-returning lambdas
    if in_rustbuffer_lambda and stripped.startswith('return C.uniffi_'):
        # Wrap return value with rustBufferFromC
        line = line.replace('return C.uniffi_', 'return rustBufferFromC(C.uniffi_')
        # Add closing paren before the status arg end
        # The line ends with `, status)` — we need `), status)` -> no, we need to close after the whole C call
        # Pattern: return C.uniffi_xxx(args, status)
        # We need: return rustBufferFromC(C.uniffi_xxx(args, status))
        # Find last `)` and add another `)` before it? No — the line has a single closing.
        # Actually: `return C.uniffi_xxx(arg1, arg2, status)` 
        # becomes: `return rustBufferFromC(C.uniffi_xxx(arg1, arg2, status))`
        # We already added the opening paren. Need to add closing paren at end.
        line = line.rstrip()
        if line.endswith(')'):
            line = line + ')'
    
    if in_rustbuffer_lambda and stripped == '}':
        # Check if this closes the lambda (rough heuristic)
        if stripped == '}' and not stripped.startswith('//'):
            in_rustbuffer_lambda = False
    
    # Fix: RustBuffer args passed to C functions
    # These are typically .Lower() results in C function call arguments
    if 'C.uniffi_rustpushgo_fn_' in stripped or 'C.uniffi_rustpushgo_checksum_' in stripped:
        # Find all INSTANCE.Lower(...) calls that produce RustBuffer values
        # and wrap them with rustBufferToC()
        # Patterns: FfiConverterXxxINSTANCE.Lower(xxx) or FfiConverterXxx{}.Lower(xxx)
        line = re.sub(
            r'(FfiConverter\w+(?:INSTANCE|{}))\.Lower\(([^)]+)\)',
            lambda m: f'rustBufferToC({m.group(1)}.Lower({m.group(2)}))',
            line
        )
        # Also handle FfiConverterBytes specifically
        line = re.sub(
            r'(FfiConverterBytesINSTANCE)\.Lower\(([^)]+)\)',
            lambda m: f'rustBufferToC({m.group(1)}.Lower({m.group(2)}))',
            line
        )
    
    # Fix: Callback return values
    if 'outBuf *C.RustBuffer' in stripped:
        pass  # These are pointer params, no conversion needed
    
    # Fix: FfiConverterCallbackInterface register/lower/etc that uses RustBuffer
    if 'func() C.RustBuffer {' in stripped and 'rustCall' not in stripped:
        pass  # These are fine as they produce C.RustBuffer directly
    
    result.append(line)

content = '\n'.join(result)

# Also need to handle: in rustCallWithError[RustBuffer] lambdas  
# that call return C.uniffi_ -- same pattern
lines = content.split('\n')
result = []
in_error_lambda = False

for i, line in enumerate(lines):
    stripped = line.strip()
    
    if 'func(status *C.RustCallStatus) RustBuffer {' in stripped:
        in_error_lambda = True
    if in_error_lambda and stripped.startswith('return rustBufferFromC('):
        pass  # Already fixed
    elif in_error_lambda and stripped.startswith('return C.uniffi_'):
        line = line.replace('return C.uniffi_', 'return rustBufferFromC(C.uniffi_')
        line = line.rstrip()
        if line.endswith(')'):
            line = line + ')'
    if in_error_lambda and stripped == '}':
        in_error_lambda = False
    
    result.append(line)

content = '\n'.join(result)

# Fix remaining: callback interface's handleMap.remove returns RustBuffer{} 
# which is fine with the new struct definition

# Fix: The FfiConverterCallbackInterface Lower method which calls
#   rustCall[RustBuffer](...) but the inner function returns C.RustBuffer
# The lambda is `func(status *C.RustCallStatus) C.RustBuffer {`
# This returns C.RustBuffer to rustCall, but rustCall expects RustBuffer
# Wait -- if rustCall[RustBuffer] is generic, the lambda should return RustBuffer
# Let me check... actually in the generated code the lambda signature IS
# `func(status *C.RustCallStatus) RustBuffer {` with alias, they're the same type.
# With our change, we need these lambdas to return RustBuffer.

# Fix callback interface Lower methods where C function returns C.RustBuffer
# in a RustBuffer context
lines = content.split('\n')
result = []
for i, line in enumerate(lines):
    # Lines like: `return C.uniffi_rustpushgo_fn_xxx(...)`
    # where the enclosing func returns RustBuffer (our type)
    # These should already be caught by the lambda detection above
    # But let's also catch the FfiConverterCallbackInterface.Lower method
    if 'handleMap.remove(handle)' in line:
        pass  # Returns RustBuffer{}, this is fine
    
    result.append(line)

content = '\n'.join(result)

with open(FILE, 'w') as f:
    f.write(content)

print("Python patch complete")
PYEOF "$FILE"

echo "Patch complete!"
