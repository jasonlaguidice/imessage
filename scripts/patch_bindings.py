#!/usr/bin/env python3
"""Patch uniffi-generated Go bindings for Go 1.24+ compatibility."""
import re
import sys

def patch(content: str) -> str:
    # 1. Add CGO LDFLAGS (platform-specific)
    content = content.replace(
        '// #include <rustpushgo.h>\nimport "C"',
        '// #include <rustpushgo.h>\n'
        '// #cgo LDFLAGS: -L${SRCDIR}/../../ -lrustpushgo -ldl -lm -lz\n'
        '// #cgo darwin LDFLAGS: -framework Security -framework SystemConfiguration -framework CoreFoundation -framework Foundation -framework CoreServices -lresolv\n'
        '// #cgo linux LDFLAGS: -lpthread -lssl -lcrypto -lresolv\n'
        'import "C"'
    )

    # 2. Replace type alias with named struct + conversion functions
    content = content.replace(
        'type RustBuffer = C.RustBuffer',
        '''type RustBuffer struct {
\tcapacity C.int32_t
\tlen      C.int32_t
\tdata     *C.uint8_t
}

func rustBufferToC(rb RustBuffer) C.RustBuffer {
\treturn *(*C.RustBuffer)(unsafe.Pointer(&rb))
}

func rustBufferFromC(crb C.RustBuffer) RustBuffer {
\treturn *(*RustBuffer)(unsafe.Pointer(&crb))
}''',
        1
    )

    # 3. Fix specific known patterns

    # Free: cb is RustBuffer, needs C.RustBuffer
    content = content.replace(
        'C.ffi_rustpushgo_rustbuffer_free(cb, status)',
        'C.ffi_rustpushgo_rustbuffer_free(rustBufferToC(cb), status)'
    )

    # Alloc/from_bytes: returns C.RustBuffer, need RustBuffer
    content = content.replace(
        'return C.ffi_rustpushgo_rustbuffer_from_bytes(foreign, status)',
        'return rustBufferFromC(C.ffi_rustpushgo_rustbuffer_from_bytes(foreign, status))'
    )

    # status.errorBuf: C.RustBuffer → RustBufferI
    content = content.replace(
        'converter.Lift(status.errorBuf)',
        'converter.Lift(rustBufferFromC(status.errorBuf))'
    )
    content = content.replace(
        'FfiConverterStringINSTANCE.Lift(status.errorBuf)',
        'FfiConverterStringINSTANCE.Lift(rustBufferFromC(status.errorBuf))'
    )

    # rust_future_complete_rust_buffer: returns C.RustBuffer, need RustBufferI
    content = content.replace(
        'return C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status)',
        'return rustBufferFromC(C.ffi_rustpushgo_rust_future_complete_rust_buffer(unsafe.Pointer(handle), status))'
    )

    # 4. Now handle .Lower() → rustBufferToC and C function returns → rustBufferFromC
    # We need to identify which FfiConverters return RustBuffer (not scalars/pointers)
    
    # These converters return RustBuffer from .Lower():
    rb_converter_prefixes = [
        'FfiConverterString',
        'FfiConverterBytes', 
        'FfiConverterType',     # FfiConverterTypeWrappedConversation, etc.
        'FfiConverterOptional',  # FfiConverterOptionalString, etc.
        'FfiConverterSequence',  # FfiConverterSequenceString, etc.
    ]
    
    # Build a regex pattern for RustBuffer-returning converters
    rb_pattern = '|'.join(rb_converter_prefixes)
    
    # Wrap .Lower() calls from RustBuffer converters with rustBufferToC()
    # But ONLY when they appear as arguments to C function calls (inside C.uniffi_ or C.ffi_ calls)
    # To handle multi-line calls, we'll process the content as a whole
    
    # Strategy: Find all `C.uniffi_rustpushgo_fn_` and `C.uniffi_rustpushgo_checksum_` call sites
    # and within their argument lists, wrap RustBuffer .Lower() calls
    
    # Actually simpler: just wrap ALL RustBuffer .Lower() calls that are NOT inside method definitions
    # The .Lower() method definitions look like: `func (c FfiConverterXxx) Lower(value Xxx) RustBuffer {`
    # The call sites look like: `FfiConverterXxxINSTANCE.Lower(val)` or `FfiConverterXxx{}.Lower(val)`
    
    lines = content.split('\n')
    result = []
    in_lower_method_def = False
    
    for line in lines:
        stripped = line.strip()
        
        # Skip method definitions
        if stripped.startswith('func (') and ') Lower(' in stripped:
            in_lower_method_def = True
        
        if not in_lower_method_def:
            # Wrap RustBuffer .Lower() calls in non-definition contexts
            for prefix in rb_converter_prefixes:
                # Match INSTANCE.Lower(xxx) pattern
                pattern = rf'({prefix}\w*INSTANCE\.Lower\([^)]*\))'
                if re.search(pattern, line):
                    line = re.sub(pattern, r'rustBufferToC(\1)', line)
                # Match {}.Lower(xxx) pattern  
                pattern2 = rf'({prefix}\w*' + r'\{\}\.Lower\([^)]*\))'
                if re.search(pattern2, line):
                    line = re.sub(pattern2, r'rustBufferToC(\1)', line)
        
        if in_lower_method_def and stripped == '}':
            in_lower_method_def = False
        
        result.append(line)
    
    content = '\n'.join(result)
    
    # 5. Fix C function returns that return C.RustBuffer in RustBuffer-returning contexts
    # These are inside `func(status *C.RustCallStatus) RustBuffer {` lambdas
    # The return statements call C.uniffi_... functions
    
    lines = content.split('\n')
    result = []
    expect_rustbuffer_return = False
    brace_depth = 0
    multi_line_wrap_pending = False
    paren_depth_for_wrap = 0
    
    for line in lines:
        stripped = line.strip()
        
        # Handle pending multi-line rustBufferFromC wrap
        if multi_line_wrap_pending:
            open_parens = line.count('(') - line.count(')')
            paren_depth_for_wrap += open_parens
            if paren_depth_for_wrap <= 0:
                # Close the rustBufferFromC paren after the last closing paren of the C call
                line = line.rstrip()
                # Find the last ')' and add our closing ')' after it
                last_paren = line.rfind(')')
                if last_paren >= 0:
                    line = line[:last_paren+1] + ')' + line[last_paren+1:]
                multi_line_wrap_pending = False
                paren_depth_for_wrap = 0
        
        # Detect RustBuffer-returning lambda start (RustBuffer or RustBufferI)
        if ('func(status *C.RustCallStatus) RustBuffer {' in stripped or
            'func(_uniffiStatus *C.RustCallStatus) RustBuffer {' in stripped or
            'func(status *C.RustCallStatus) RustBufferI {' in stripped or
            'func(_uniffiStatus *C.RustCallStatus) RustBufferI {' in stripped or
            'func(handle *C.void, status *C.RustCallStatus) RustBufferI {' in stripped):
            expect_rustbuffer_return = True
            brace_depth = 1
        elif expect_rustbuffer_return:
            brace_depth += stripped.count('{') - stripped.count('}')
            if brace_depth <= 0:
                expect_rustbuffer_return = False
        
        if expect_rustbuffer_return and stripped.startswith('return C.') and 'rustBufferFromC' not in stripped:
            # This C function call returns C.RustBuffer but we need RustBuffer
            # Mark this line for wrapping - we need to find where the call ends
            # (could be multi-line)
            indent = line[:len(line) - len(line.lstrip())]
            new_return = 'return rustBufferFromC(' + stripped[len('return '):]
            # Count parens in the ORIGINAL return statement (without our wrapper)
            orig_open = stripped[len('return '):].count('(') - stripped[len('return '):].count(')')
            if orig_open <= 0:
                # Single-line call, close our wrapper
                line = indent + new_return + ')'
            else:
                # Multi-line call - track remaining parens to know when to close wrapper
                line = indent + new_return
                multi_line_wrap_pending = True
                paren_depth_for_wrap = orig_open
        
        result.append(line)
    
    content = '\n'.join(result)
    
    # 6. Fix *outBuf assignments where RustBuffer needs to be C.RustBuffer
    # Pattern: `*outBuf = FfiConverter...INSTANCE.drop(...)` 
    content = re.sub(
        r'\*outBuf = (FfiConverter\w+INSTANCE\.drop\([^)]*\))',
        r'*outBuf = rustBufferToC(\1)',
        content
    )
    
    # 7. Fix double-wrapping
    while 'rustBufferToC(rustBufferToC(' in content:
        content = content.replace('rustBufferToC(rustBufferToC(', 'rustBufferToC(')
    while 'rustBufferFromC(rustBufferFromC(' in content:
        content = content.replace('rustBufferFromC(rustBufferFromC(', 'rustBufferFromC(')
    
    return content

if __name__ == '__main__':
    path = sys.argv[1] if len(sys.argv) > 1 else 'pkg/rustpushgo/rustpushgo.go'
    with open(path) as f:
        content = f.read()
    result = patch(content)
    with open(path, 'w') as f:
        f.write(result)
    print(f"Successfully patched {path}")
