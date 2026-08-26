/**
 * New Frontend System exports (current Backstage default). Pages are exposed
 * under /alpha per contemporary convention; the same components serve the
 * classic entry point.
 */

import React from 'react';
import { useParams } from 'react-router-dom';
import {
  PageBlueprint,
  createFrontendPlugin,
} from '@backstage/frontend-plugin-api';
import { InventoryPage } from './components/InventoryPage';
import { ResourceDetailPage } from './components/ResourceDetailPage';
import { CreateResourcePage } from './components/CreateResourcePage';
import { ResourceTypeDetailPage, ResourceTypesPage } from './components/ResourceTypesPage';
import { OperationPage } from './components/OperationPage';

const ResourceDetailBridge: React.FC = () => {
  const { id } = useParams<{ id: string }>();
  return <ResourceDetailPage resourceId={id ?? ''} />;
};

const ResourceTypeDetailBridge: React.FC = () => {
  const { name, version } = useParams<{ name: string; version: string }>();
  return <ResourceTypeDetailPage name={name ?? ''} version={version ?? ''} />;
};

const OperationBridge: React.FC = () => <OperationPage />;

/** Convert classic plugin to new frontend system. */
export default createFrontendPlugin({
  pluginId: 'liftr',
  extensions: [
    PageBlueprint.make({
      name: 'resource-types',
      params: {
        path: '/liftr/resource-types',
        title: 'Liftr Resource Types',
        loader: async () => <ResourceTypesPage />,
      },
    }),
    PageBlueprint.make({
      params: {
        path: '/liftr',
        title: 'Liftr',
        loader: async () => <InventoryPage />,
      },
    }),
    PageBlueprint.make({
      name: 'create',
      params: {
        path: '/liftr/create',
        title: 'Create Liftr Resource',
        loader: async () => <CreateResourcePage />,
      },
    }),
    PageBlueprint.make({
      name: 'operation',
      params: {
        path: '/liftr/operations/:id',
        title: 'Liftr Operation',
        loader: async () => <OperationBridge />,
      },
    }),
    PageBlueprint.make({
      name: 'resource',
      params: {
        path: '/liftr/resources/:id',
        title: 'Liftr Resource',
        loader: async () => <ResourceDetailBridge />,
      },
    }),
    PageBlueprint.make({
      name: 'resource-type',
      params: {
        path: '/liftr/resource-types/:name/:version',
        title: 'Liftr Resource Type',
        loader: async () => <ResourceTypeDetailBridge />,
      },
    }),
  ],
});
