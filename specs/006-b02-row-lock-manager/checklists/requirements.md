# Specification Quality Checklist: B02 Row Lock Manager

**Purpose**: specify/clarify 阶段规格质量审核，不表示实现或测试通过。
**Created**: 2026-10-10
**Feature**: [spec.md](../spec.md)

## Content Quality
- [x] 无语言、框架或具体实现算法细节；内核领域语义及既有兼容约束属于需求。
- [x] 以使用者并发、等待、读取、回滚与诊断价值为中心。
- [x] 用户场景以 Given/When/Then 描述，专业术语在实体/场景中有定义。
- [x] 官方 spec-template 必需章节完整。

## Requirement Completeness
- [x] 无待澄清占位符；所有默认边界显式记录。
- [x] FR-001..032 可测试、无互斥语义矛盾。
- [x] SC-001..008 有可计数/可观察标准。
- [x] 成功标准不依赖特定实现框架。
- [x] 六个故事都有独立测试与验收场景。
- [x] 未命中、墓碑、升级、端点、世代、取消和资源失败边界已识别。
- [x] Scope、非目标、有限 InnoDB-style 兼容明确。
- [x] B01、A05、源码基线、后端能力及默认值均有记录。

## Feature Readiness
- [x] 每条 FR 有验收含义；实施/测试映射在 tasks.md 中进一步审核。
- [x] 场景覆盖 Record/IS/IX/Gap/Next-Key/II、SQL、DML、生命周期、诊断。
- [x] 规格提供验证 SC 的观察条件；本轮不声称运行验收。
- [x] 规格未包含生产代码或接口实现。

## Notes
审核范围仅为文档。锁定 SELECT 可以读较新版本，原快照 API 不变；DML 仍按原 readTS
冲突并要求 root 重试。这是已说明的兼容限制，不是遗漏的 locked-write 豁免。
