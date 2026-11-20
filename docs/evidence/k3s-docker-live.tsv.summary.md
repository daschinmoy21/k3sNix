C / H / E = churn-v1 / hello / empty states; seed states rotate across fixed worker identities.
Free distribution counts choices; observed means describe the recorded runs per target/style, not a scheduler expectation.
Pinned costs measure each state once per target/style. Raw = NAR / whole-image layer tar bytes.
K3snix ties are listed; the first node alphabetically is copied to.

### Raw bytes

| Target | k3snix choice (ties) | k3snix bytes | Fine free C/H/E | Fine observed mean | Fine pinned C/H/E | Coarse free C/H/E | Coarse observed mean | Coarse pinned C/H/E |
|---|---|---:|---|---:|---|---|---:|---|
| churn-v1 | churn-v1 | 0 | 1 / 3 / 2 | 7,017,813.3 | 0 / 8,417,280 / 8,427,520 | 2 / 1 / 3 | 5,602,986.7 | 0 / 8,396,800 / 8,407,040 |
| churn-v2 | churn-v1 | 206,248 | 2 / 1 / 3 | 5,838,506.7 | 235,520 / 8,632,320 / 8,642,560 | 3 / 1 / 2 | 8,605,013.3 | 8,601,600 / 8,601,600 / 8,611,840 |
| churn-v3 | churn-v1 | 485,336 | 2 / 2 / 2 | 6,123,520.0 | 522,240 / 8,919,040 / 8,929,280 | 0 / 3 / 3 | 8,893,440.0 | 8,888,320 / 8,888,320 / 8,898,560 |
| churn-v4 | churn-v1 | 846,344 | 2 / 3 / 1 | 6,490,453.3 | 890,880 / 9,287,680 / 9,297,920 | 2 / 2 / 2 | 9,250,133.3 | 9,246,720 / 9,246,720 / 9,256,960 |
| share-a | hello | 0 | 5 / 0 / 1 | 38,729,386.7 | 38,727,680 / 0 / 38,737,920 | 4 / 2 / 0 | 25,804,800.0 | 38,707,200 / 0 / 38,717,440 |
| share-b | hello | 13,334,096 | 0 / 2 / 4 | 39,069,013.3 | 51,855,360 / 13,475,840 / 51,865,600 | 2 / 2 / 2 | 51,797,333.3 | 51,793,920 / 51,793,920 / 51,804,160 |
| unrelated-a | churn-v1,empty,hello | 65,648 | 1 / 2 / 3 | 76,800.0 | 71,680 / 71,680 / 81,920 | 4 / 1 / 1 | 73,386.7 | 71,680 / 71,680 / 81,920 |
| unrelated-b | churn-v1,empty,hello | 98,416 | 2 / 2 / 2 | 105,813.3 | 102,400 / 102,400 / 112,640 | 2 / 3 / 1 | 104,106.7 | 102,400 / 102,400 / 112,640 |

### Actual wire bytes (agent uncompressed; registry gzip)

| Target | k3snix choice (ties) | k3snix bytes | Fine free C/H/E | Fine observed mean | Fine pinned C/H/E | Coarse free C/H/E | Coarse observed mean | Coarse pinned C/H/E |
|---|---|---:|---|---:|---|---|---:|---|
| churn-v1 | churn-v1 | 0 | 1 / 3 / 2 | 10,211.8 | 0 / 12,213 / 12,316 | 2 / 1 / 3 | 8,007.5 | 0 / 11,934 / 12,037 |
| churn-v2 | churn-v1 | 206,248 | 2 / 1 / 3 | 8,877.5 | 1,036 / 12,721 / 12,824 | 3 / 1 / 2 | 12,406.3 | 12,372 / 12,372 / 12,475 |
| churn-v3 | churn-v1 | 485,336 | 2 / 2 / 2 | 9,435.3 | 1,611 / 13,296 / 13,399 | 0 / 3 / 3 | 12,947.5 | 12,896 / 12,896 / 12,999 |
| churn-v4 | churn-v1 | 846,344 | 2 / 3 / 1 | 10,073.2 | 2,266 / 13,951 / 14,054 | 2 / 2 / 2 | 13,559.3 | 13,525 / 13,525 / 13,628 |
| share-a | hello | 0 | 5 / 0 / 1 | 13,974,941.2 | 13,974,924 / 0 / 13,975,027 | 4 / 2 / 0 | 9,311,524.0 | 13,967,286 / 0 / 13,967,389 |
| share-b | hello | 13,334,096 | 0 / 2 / 4 | 14,379,374.0 | 19,011,666 / 5,114,584 / 19,011,769 | 2 / 2 / 2 | 19,008,275.3 | 19,008,241 / 19,008,241 / 19,008,344 |
| unrelated-a | churn-v1,empty,hello | 65,648 | 1 / 2 / 3 | 333.5 | 282 / 282 / 385 | 4 / 1 / 1 | 299.2 | 282 / 282 / 385 |
| unrelated-b | churn-v1,empty,hello | 98,416 | 2 / 2 / 2 | 331.3 | 297 / 297 / 400 | 2 / 3 / 1 | 314.2 | 297 / 297 / 400 |

192/192 live measurements matched the model and restored seeded state (48 k3snix, 96 free, 48 pinned).
