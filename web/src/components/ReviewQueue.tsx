import { useState, type ReactNode } from 'react';
import { Alert, Button, Card, Descriptions, Form, Input, Modal, Space, Table, Tag, Typography } from 'antd';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { consoleApi } from '../api/client';
import type { ReviewAction, ReviewCursor, ReviewDisposition, ReviewExecution } from '../api/types';
import { ErrorView } from './states';
import { MicroYuan } from './Money';
import { formatTimestamp, formatAgeSeconds } from '../utils/format';

export function ReviewQueueSection(): ReactNode {
 const cache = useQueryClient();
 const [cursor, setCursor] = useState<ReviewCursor>();
 const [selected, setSelected] = useState<{item: ReviewExecution; action: ReviewAction}>();
 const [result, setResult] = useState<ReviewDisposition>();
 const [form] = Form.useForm<{reason: string}>();
 const queue = useQuery({queryKey:['platform','review',cursor], queryFn:()=>consoleApi.reviewQueue(cursor)});
 const disposition = useMutation({
  mutationFn:({item,action,reason}:{item:ReviewExecution;action:ReviewAction;reason:string})=>consoleApi.disposeReviewExecution(item.execution_id,action,reason),
  onSuccess:(response)=>{
   setResult(response);setSelected(undefined);form.resetFields();setCursor(undefined);
   void cache.invalidateQueries({queryKey:['platform','review']});
   void cache.invalidateQueries({queryKey:['org',response.organization_id]});
  },
 });
 const open = (item:ReviewExecution,action:ReviewAction):void=>{form.resetFields();disposition.reset();setSelected({item,action});};
 const submit = async ():Promise<void>=>{
  const values=await form.validateFields();
  if(selected) { try { await disposition.mutateAsync({...selected,reason:values.reason.trim()}); } catch { /* Render server error; never automatically repeat a money operation. */ } }
 };
 return <Card title="待人工处理的资金请求" data-testid="review-queue-card">
  <Space direction="vertical" style={{width:'100%'}} size={12}>
   <Alert type="info" showIcon message="此处操作会实际移动资金" description="释放预占：平台承担未知成本，客户不扣款。人工结算：按请求准入时的预占金额扣款，并释放预占。异常页的记录决议不会替代这里的资金处理。" />
   <Space><Button onClick={()=>{setCursor(undefined);void queue.refetch();}}>刷新队列</Button>{cursor?<Button onClick={()=>setCursor(undefined)}>返回首页</Button>:null}</Space>
   {queue.isError?<ErrorView error={queue.error} title="待处理队列读取失败"/>:null}
   {queue.data?<Typography.Text>待处理 {queue.data.totals.open} 笔 · 超过 24 小时 {queue.data.totals.sla_breaches} 笔 · 最长等待 {formatAgeSeconds(queue.data.totals.oldest_age_seconds)}</Typography.Text>:null}
   <Table<ReviewExecution> rowKey="execution_id" size="small" loading={queue.isPending} dataSource={queue.data?.items??[]} pagination={false} scroll={{x:900}} columns={[
    {title:'请求',dataIndex:'execution_id'}, {title:'组织',dataIndex:'organization_id'}, {title:'模型',dataIndex:'model'},
    {title:'预占金额',render:(_,row)=><MicroYuan value={row.reserved_micro}/>},
    {title:'进入队列',render:(_,row)=><Space direction="vertical"><span>{formatTimestamp(row.updated_at)}</span>{row.sla_breached?<Tag color="red">已超时</Tag>:null}</Space>},
    {title:'原因',dataIndex:'reason'},
    {title:'操作',render:(_,row)=><Space><Button disabled={row.age_seconds<(queue.data?.min_age_seconds??Infinity)} onClick={()=>open(row,'release')}>释放预占</Button><Button danger disabled={row.age_seconds<(queue.data?.min_age_seconds??Infinity)} onClick={()=>open(row,'settle')}>人工结算</Button></Space>},
   ]}/>
   {queue.data?.next_cursor?<Button onClick={()=>setCursor(queue.data!.next_cursor!)}>下一页</Button>:null}
   <Typography.Text type="secondary">安全等待窗口内不能操作；服务端会再次校验请求状态和收据。并发处理或重复提交会被拒绝，刷新后查看结果。</Typography.Text>
   {result?<Alert type="success" showIcon message={`请求 ${result.execution_id} 已${result.action==='release'?'释放预占':'人工结算'}`} description={<Descriptions size="small" column={1}>
    <Descriptions.Item label="实际扣款"><MicroYuan value={result.charge_micro}/></Descriptions.Item>
    <Descriptions.Item label="处理后可用余额"><MicroYuan value={result.account.available_micro}/></Descriptions.Item>
    <Descriptions.Item label="处理后剩余预占"><MicroYuan value={result.account.reserved_micro}/></Descriptions.Item>
    <Descriptions.Item label="扣款 / 释放分录">{result.debit_entry?.id??'无扣款'} / {result.release_entry?.id??'无预占'}</Descriptions.Item>
    <Descriptions.Item label="状态 / 操作编号">{result.status} / {result.request_id}</Descriptions.Item>
   </Descriptions>}/>:null}
  </Space>
  <Modal title={selected?.action==='settle'?'确认人工结算':'确认释放预占'} open={!!selected} confirmLoading={disposition.isPending} onOk={()=>void submit()} onCancel={()=>{if(!disposition.isPending)setSelected(undefined);}} okText={selected?.action==='settle'?'确认扣款并结清':'确认释放，不向客户扣款'} cancelText="取消" closable={!disposition.isPending} maskClosable={!disposition.isPending}>
   {selected?<><p>请求 {selected.item.execution_id} · 组织 {selected.item.organization_id}</p><p>{selected.action==='settle'?'将按准入预占金额向客户扣款：':'将释放预占，平台承担未知成本：'}<MicroYuan value={selected.item.reserved_micro}/></p></>:null}
   <Form form={form} layout="vertical"><Form.Item name="reason" label="处理理由（记录到审计）" rules={[{required:true,whitespace:true,max:512}]}><Input.TextArea rows={3} maxLength={512}/></Form.Item></Form>
   {disposition.isError?<ErrorView error={disposition.error} title="处理未确认，请刷新队列核实结果"/>:null}
  </Modal>
 </Card>;
}
