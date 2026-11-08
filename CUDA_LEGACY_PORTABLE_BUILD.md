# CUDA Legacy para o Ollama Portable

## Objetivo

Construir um backend CUDA adicional e coerente com este checkout para a build
portátil do Ollama, sem recompilar ou substituir preventivamente o Desktop, o
servidor Go, o `llama-server.exe`, o runtime Vulkan ou o runtime CUDA 12 atual.

O pacote cobre somente estas GPUs:

| GPU | Família | Compute capability | Arquitetura compilada |
| --- | --- | ---: | --- |
| NVIDIA GTX 1050 Ti | Pascal | 6.1 | `sm_61` |
| NVIDIA RTX 2060 SUPER | Turing | 7.5 | `sm_75` |

O CUDA 13 não deve ser usado neste build porque não oferece o alvo Pascal
necessário. Use um CUDA Toolkit 12.x cujo `nvcc --list-gpu-arch` anuncie
`compute_61` e `compute_75`.

## Contrato de compatibilidade

O backend precisa ser compilado a partir do mesmo checkout e do mesmo
`LLAMA_CPP_VERSION` usados pelo núcleo nativo da build portátil. Não copie um
`ggml-cuda.dll` de outra versão e não misture bibliotecas GGML produzidas por
commits diferentes.

O pacote gerado possui esta árvore:

```text
package\
└── runtimes\
    └── cuda_legacy\
        ├── ggml-cuda.dll
        ├── cudart64_*.dll
        ├── cublas64_*.dll
        ├── cublasLt64_*.dll
        ├── CUDA-LEGACY-BUILD-INFO.txt
        ├── CUDA-ARCHITECTURES.txt
        ├── DLL-DEPENDENCIES.txt
        ├── SHA256SUMS.txt
        └── demais dependências instaladas pelo CMake
```

Ao mesclar `package` na raiz do Ollama Portable, somente
`runtimes\cuda_legacy` deve ser adicionado. Preserve:

- `Ollama.exe`;
- `runtimes\ollama-serve.exe`;
- `runtimes\llama-server.exe` e as DLLs GGML base;
- `runtimes\vulkan` e `runtimes\cuda_v12`;
- `database`, `chats`, `mcp` e `models`.

A cópia portátil atualmente usada para os testes está em:

```text
D:\Projects\ollama-p-coherent-test
```

Trate esse diretório como alvo de validação, não como diretório de build. Gere
primeiro o ZIP em `D:\OllamaBuild\cuda-legacy`, preserve uma cópia do portátil
funcional e somente então mescle `package\runtimes\cuda_legacy` no alvo.

## Pré-requisitos

- Windows x64;
- Visual Studio 2022 com **Desktop development with C++**;
- CMake;
- Ninja;
- CUDA Toolkit 12.x;
- espaço de compilação em `D:`.

O script força build, stage e temporários para `D:`. Ele pode usar um Toolkit
já instalado em outro volume, mas não baixa nem instala dependências.

## Verificação sem compilar

Na raiz do repositório:

```powershell
pwsh -ExecutionPolicy Bypass -File .\scripts\build_cuda_legacy_windows.ps1 -CheckOnly
```

Quando o Toolkit estiver em um caminho não detectado automaticamente:

```powershell
pwsh -ExecutionPolicy Bypass -File .\scripts\build_cuda_legacy_windows.ps1 `
  -ToolkitRoot "D:\NVIDIA\CUDA\v12.9" `
  -CheckOnly
```

## Compilação

```powershell
pwsh -ExecutionPolicy Bypass -File .\scripts\build_cuda_legacy_windows.ps1 `
  -WorkRoot "D:\OllamaBuild\cuda-legacy" `
  -CudaArchitectures "61;75" `
  -Jobs 4
```

Resultados:

```text
D:\OllamaBuild\cuda-legacy\package
D:\OllamaBuild\cuda-legacy\ollama-portable-cuda-legacy-sm61-sm75.zip
```

O script usa `cuobjdump --list-elf` e falha se o `ggml-cuda.dll` produzido não
contiver ambos os alvos, `sm_61` e `sm_75`.

## Preset CMake equivalente

O repositório também possui o preset:

```powershell
Push-Location .\llama\server
cmake --preset llama_cuda_legacy_windows
cmake --build --preset llama_cuda_legacy_windows --parallel 4
Pop-Location
```

O script é preferível para o pacote portátil porque também seleciona o Toolkit,
mantém temporários em `D:`, instala as dependências, valida o fat binary e cria o
ZIP na árvore correta.

## Ambiente confirmado

Em 8 de novembro de 2026, a execução de `-CheckOnly` confirmou:

```text
CUDA Toolkit: 12.6
llama.cpp version: b10488
GTX 1050 Ti: compute capability 6.1
RTX 2060 SUPER: compute capability 7.5
Build, stage e temporários: D:\OllamaBuild\cuda-legacy
```

Isso confirma os pré-requisitos e os alvos de compilação. Ainda não confirma o
carregamento do backend nem a inferência; essas provas dependem do build e do
teste dentro do portátil.

## Validação de runtime

No computador atual, valide primeiro a RTX 2060 SUPER:

1. Preserve uma cópia de `D:\Projects\ollama-p-coherent-test`.
2. Mescle o conteúdo de `package` na raiz da cópia de teste.
3. Selecione **CUDA Legacy** na interface.
4. Confirme que o servidor reiniciou com `OLLAMA_LLM_LIBRARY=cuda_legacy`.
5. Confirme que o dispositivo exibido é a RTX 2060 SUPER.
6. Carregue um modelo e faça uma inferência curta.
7. Confirme `/api/ps` com `size_vram` maior que zero e atividade real da GPU.

A presença do runtime no seletor não prova que ele carregou. Os logs, a
enumeração do dispositivo, `size_vram` e uma inferência real são as evidências.

## Compilação no GitHub Actions

O workflow `.github/workflows/cuda-legacy-portable.yaml` usa `windows-2022`
somente na branch `codex/cuda-legacy-windows-actions`. Não cria releases, não
altera `main` e possui apenas permissão de leitura do conteúdo do repositório.
O primeiro push executa somente a verificação dos pré-requisitos. A compilação
é habilitada por um push posterior com `[build-cuda-legacy]` na mensagem do
commit e alteração em um dos arquivos cobertos pelo filtro `paths` do workflow.
O disparo manual também aceita `build: true`, quando o GitHub disponibilizar
`workflow_dispatch` para esse workflow; ele não é necessário para o fluxo via push.

O runner precisa ter Actions habilitado e permissão para executar
`actions/checkout` e `actions/upload-artifact`. A verificação prévia exige
Windows x64, pelo menos 7 GiB de RAM e 8 GiB livres em `D:`, CMake, Ninja e
Visual Studio 2022. O runner padrão não possui GPU NVIDIA: compilação e inspeção
dos binários são possíveis, mas inferência física continua pendente nas GPUs alvo.

`scripts/install_cuda_legacy_ci.ps1` prepara CUDA **12.6.3** em `D:` a partir dos
arquivos oficiais NVIDIA, com SHA-256 fixado para cada componente. Instala apenas
NVCC, cudart, CCCL, cuobjdump e cuBLAS; não instala driver. NVCC/CCCL são ferramentas
de build, não entram no ZIP. Downloads, temporários, fontes nativas, build e stage
permanecem em `D:`. São usados dois jobs de build, com dois threads internos do NVCC.

A identidade nativa é conferida contra o handoff
`f0b602e29f782180b8c3da517644b82672bba610`, `LLAMA_CPP_VERSION=b10488` e o
commit llama.cpp `9d77fa17254e1dee4b9e92504c91611a60b1359f`.
Nenhuma versão de biblioteca nativa é atualizada. O único alvo solicitado ao
CMake é `ggml-cuda`, junto das dependências de link exigidas por esse alvo.
Não são compilados o Desktop, o servidor Go ou `llama-server.exe`.

Antes de publicar o artefato de teste, o script verifica:

- código de máquina CUDA para `sm_61` e `sm_75` via `cuobjdump`;
- formato PE x64 de todas as DLLs;
- exportação `ggml_backend_init`;
- dependências importadas, exigindo as DLLs redistribuíveis no pacote;
- ausência de executáveis e de DLLs GGML base no runtime adicional;
- entradas do ZIP exclusivamente sob `runtimes/cuda_legacy/`.

As bibliotecas GGML base são fornecidas pelo portátil; `nvcuda.dll` é fornecida
pelo driver NVIDIA instalado na máquina de teste. O pacote não inclui essas DLLs.
O relatório registra importações, versões e hashes; a compatibilidade com os
binários realmente instalados e a inferência só ficam confirmadas após o teste
na cópia do portátil descrita acima.

O artefato **ollama-portable-cuda-legacy-sm61-sm75**, retido por 14 dias, contém
o ZIP do runtime e seu SHA-256. O GitHub entrega artefatos dentro de um ZIP de
transporte: extraia esse invólucro primeiro. O ZIP interno é o pacote a extrair
na cópia de teste do portátil e contém somente `runtimes/cuda_legacy/`.
O artefato separado `cuda-legacy-reports-*` contém os relatórios do processo.

As duas GPUs foram detectadas pelo `-CheckOnly`, portanto o teste físico deve
validar Turing e Pascal. A compilação, por si só, não prova que ambos os
dispositivos carregam o mesmo backend corretamente.

## Limites deste trabalho

- Não inclui Maxwell/GTX 970 (`sm_52`).
- Não altera o runtime CUDA 12 existente.
- Não altera fallback de CPU, auto-fit ou distribuição de camadas.
- Não inclui modelos, banco SQLite, chats ou servidores MCP.
- Não publica binários no Git; o ZIP é um artefato local de compilação.

## Instruções para outro Codex

Antes de executar o build:

1. leia este documento e `AGENTS.md`;
2. preserve alterações locais existentes;
3. confirme o commit e `LLAMA_CPP_VERSION`;
4. execute primeiro `-CheckOnly`;
5. não instale CUDA 13 nem compile o Desktop inteiro;
6. não use `C:` para build, stage ou temporários grandes;
7. reporte o caminho do ZIP e quais validações foram realmente executadas.
