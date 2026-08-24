import React from 'react';
import { Route, Routes, useParams } from 'react-router-dom';
import { InventoryPage } from './components/InventoryPage';
import { ResourceDetailPage } from './components/ResourceDetailPage';
import { CreateResourcePage } from './components/CreateResourcePage';
import { ResourceTypeDetailPage } from './components/ResourceTypesPage';
import { OperationPage } from './components/OperationPage';

const ResourceDetailBridge: React.FC = () => {
  const { id } = useParams<{ id: string }>();
  return <ResourceDetailPage resourceId={id ?? ''} />;
};

const ResourceTypeDetailBridge: React.FC = () => {
  const { name, version } = useParams<{ name: string; version: string }>();
  return <ResourceTypeDetailPage name={name ?? ''} version={version ?? ''} />;
};

export const LiftrRouter: React.FC = () => <LiftrRouterView />;

const LiftrRouterView = (): JSX.Element => (

  <Routes>
    <Route path="/" element={<InventoryPage />} />
    <Route path="/create" element={<CreateResourcePage />} />
    <Route path="/resources/:id" element={<ResourceDetailBridge />} />
    <Route path="/resource-types" element={<ResourceTypeDetailBridge />} />
    <Route path="/resource-types/:name/:version" element={<ResourceTypeDetailBridge />} />
    <Route path="/operations/:id" element={<OperationPage />} />
  </Routes>
);
