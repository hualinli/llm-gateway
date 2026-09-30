-- 被更新 revision 取代的行：status 词表里没有 superseded，published/failed 都会
-- 撒谎，因此直接删除。当前 revision 永远有自己的那一行，不会丢失待发布状态。
DELETE FROM policy_publications WHERE id = $1
